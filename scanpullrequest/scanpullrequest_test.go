package scanpullrequest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfrog/frogbot/utils"
	"github.com/jfrog/frogbot/utils/outputwriter"
	"github.com/jfrog/froggit-go/vcsclient"
	"github.com/jfrog/froggit-go/vcsutils"
	coreconfig "github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	"github.com/jfrog/jfrog-cli-core/v2/xray/commands/audit"
	"github.com/jfrog/jfrog-cli-core/v2/xray/formats"
	xrayutils "github.com/jfrog/jfrog-cli-core/v2/xray/utils"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"github.com/jfrog/jfrog-client-go/xray/services"
	"github.com/owenrumney/go-sarif/v2/sarif"
	"github.com/stretchr/testify/assert"
)

const (
	testMultiDirProjConfigPath       = "testdata/config/frogbot-config-multi-dir-test-proj.yml"
	testMultiDirProjConfigPathNoFail = "testdata/config/frogbot-config-multi-dir-test-proj-no-fail.yml"
	testProjSubdirConfigPath         = "testdata/config/frogbot-config-test-proj-subdir.yml"
	testCleanProjConfigPath          = "testdata/config/frogbot-config-clean-test-proj.yml"
	testProjConfigPath               = "testdata/config/frogbot-config-test-proj.yml"
	testProjConfigPathNoFail         = "testdata/config/frogbot-config-test-proj-no-fail.yml"
	testSourceBranchName             = "pr"
	testTargetBranchName             = "master"
)

func TestCreateVulnerabilitiesRows(t *testing.T) {
	// Previous scan with only one violation - XRAY-1
	previousScan := services.ScanResponse{
		Violations: []services.Violation{
			{
				IssueId:       "XRAY-1",
				Summary:       "summary-1",
				Severity:      "high",
				Cves:          []services.Cve{},
				ViolationType: "security",
				Components:    map[string]services.Component{"component-A": {}, "component-B": {}},
			},
			{
				IssueId:       "XRAY-4",
				ViolationType: "license",
				LicenseKey:    "Apache-2.0",
				Components:    map[string]services.Component{"Dep-2": {}},
			},
		},
	}

	// Current scan with 2 violations - XRAY-1 and XRAY-2
	currentScan := services.ScanResponse{
		Violations: []services.Violation{
			{
				IssueId:       "XRAY-1",
				Summary:       "summary-1",
				Severity:      "high",
				ViolationType: "security",
				Components:    map[string]services.Component{"component-A": {}, "component-B": {}},
			},
			{
				IssueId:       "XRAY-2",
				Summary:       "summary-2",
				ViolationType: "security",
				Severity:      "low",
				Components:    map[string]services.Component{"component-C": {}, "component-D": {}},
			},
			{
				IssueId:       "XRAY-3",
				ViolationType: "license",
				LicenseKey:    "MIT",
				Components:    map[string]services.Component{"Dep-1": {}},
			},
		},
	}

	// Run createNewIssuesRows and make sure that only the XRAY-2 violation exists in the results
	securityViolationsRows, licenseViolations, err := createNewVulnerabilitiesRows(
		&audit.Results{ExtendedScanResults: &xrayutils.ExtendedScanResults{XrayResults: []services.ScanResponse{previousScan}}},
		&audit.Results{ExtendedScanResults: &xrayutils.ExtendedScanResults{XrayResults: []services.ScanResponse{currentScan}}},
		nil,
	)
	assert.NoError(t, err)
	assert.Len(t, licenseViolations, 1)
	assert.Len(t, securityViolationsRows, 2)
	assert.Equal(t, "XRAY-2", securityViolationsRows[0].IssueId)
	assert.Equal(t, "low", securityViolationsRows[0].Severity)
	assert.Equal(t, "XRAY-2", securityViolationsRows[1].IssueId)
	assert.Equal(t, "low", securityViolationsRows[1].Severity)
	assert.Equal(t, "MIT", licenseViolations[0].LicenseKey)
	assert.Equal(t, "Dep-1", licenseViolations[0].ImpactedDependencyName)

	impactedPackageOne := securityViolationsRows[0].ImpactedDependencyName
	impactedPackageTwo := securityViolationsRows[1].ImpactedDependencyName
	assert.ElementsMatch(t, []string{"component-C", "component-D"}, []string{impactedPackageOne, impactedPackageTwo})
}

func TestCreateVulnerabilitiesRowsCaseNoPrevViolations(t *testing.T) {
	// Previous scan with no violation
	previousScan := services.ScanResponse{
		Violations: []services.Violation{},
	}

	// Current scan with 2 violations - XRAY-1 and XRAY-2
	currentScan := services.ScanResponse{
		Violations: []services.Violation{
			{
				IssueId:       "XRAY-1",
				Summary:       "summary-1",
				Severity:      "high",
				ViolationType: "security",
				Components:    map[string]services.Component{"component-A": {}},
			},
			{
				IssueId:       "XRAY-2",
				Summary:       "summary-2",
				ViolationType: "security",
				Severity:      "low",
				Components:    map[string]services.Component{"component-C": {}},
			},
			{
				IssueId:       "XRAY-3",
				ViolationType: "license",
				LicenseKey:    "MIT",
				Components:    map[string]services.Component{"Dep-1": {}},
			},
		},
	}

	expectedVulns := []formats.VulnerabilityOrViolationRow{
		{
			IssueId: "XRAY-1",
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{
				SeverityDetails:        formats.SeverityDetails{Severity: "high"},
				ImpactedDependencyName: "component-A",
			},
		},
		{
			IssueId: "XRAY-2",
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{
				SeverityDetails:        formats.SeverityDetails{Severity: "low"},
				ImpactedDependencyName: "component-C",
			},
		},
	}

	expectedLicenses := []formats.LicenseRow{
		{
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{ImpactedDependencyName: "Dep-1"},
			LicenseKey:                "MIT",
		},
	}

	// Run createNewIssuesRows and expect both XRAY-1 and XRAY-2 violation in the results
	vulnerabilities, licenses, err := createNewVulnerabilitiesRows(
		&audit.Results{ExtendedScanResults: &xrayutils.ExtendedScanResults{XrayResults: []services.ScanResponse{previousScan}}},
		&audit.Results{ExtendedScanResults: &xrayutils.ExtendedScanResults{XrayResults: []services.ScanResponse{currentScan}}},
		[]string{},
	)
	assert.NoError(t, err)
	assert.Len(t, licenses, 1)
	assert.Len(t, vulnerabilities, 2)
	assert.ElementsMatch(t, expectedVulns, vulnerabilities)
	assert.Equal(t, expectedLicenses[0].ImpactedDependencyName, licenses[0].ImpactedDependencyName)
	assert.Equal(t, expectedLicenses[0].LicenseKey, licenses[0].LicenseKey)
}

func TestGetNewViolationsCaseNoNewViolations(t *testing.T) {
	// Previous scan with 2 security violations and 1 license violation - XRAY-1 and XRAY-2
	previousScan := services.ScanResponse{
		Violations: []services.Violation{
			{
				IssueId:       "XRAY-1",
				Severity:      "high",
				ViolationType: "security",
				Components:    map[string]services.Component{"component-A": {}},
			},
			{
				IssueId:       "XRAY-2",
				Summary:       "summary-2",
				ViolationType: "security",
				Severity:      "low",
				Components:    map[string]services.Component{"component-C": {}},
			},
			{
				IssueId:       "XRAY-3",
				LicenseKey:    "MIT",
				ViolationType: "license",
				Components:    map[string]services.Component{"component-B": {}},
			},
		},
	}

	// Current scan with no violation
	currentScan := services.ScanResponse{
		Violations: []services.Violation{},
	}

	// Run createNewIssuesRows and expect no violations in the results
	securityViolations, licenseViolations, err := createNewVulnerabilitiesRows(
		&audit.Results{ExtendedScanResults: &xrayutils.ExtendedScanResults{XrayResults: []services.ScanResponse{previousScan}}},
		&audit.Results{ExtendedScanResults: &xrayutils.ExtendedScanResults{XrayResults: []services.ScanResponse{currentScan}}},
		[]string{"MIT"},
	)
	assert.NoError(t, err)
	assert.Len(t, securityViolations, 0)
	assert.Len(t, licenseViolations, 0)
}

func TestGetAllVulnerabilities(t *testing.T) {
	// Current scan with 2 vulnerabilities - XRAY-1 and XRAY-2
	currentScan := services.ScanResponse{
		Vulnerabilities: []services.Vulnerability{
			{
				IssueId:    "XRAY-1",
				Summary:    "summary-1",
				Severity:   "high",
				Components: map[string]services.Component{"component-A": {}, "component-B": {}},
			},
			{
				IssueId:    "XRAY-2",
				Summary:    "summary-2",
				Severity:   "low",
				Components: map[string]services.Component{"component-C": {}, "component-D": {}},
			},
		},
	}

	expected := []formats.VulnerabilityOrViolationRow{
		{
			Summary: "summary-1",
			IssueId: "XRAY-1",
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{
				SeverityDetails:        formats.SeverityDetails{Severity: "high"},
				ImpactedDependencyName: "component-A",
			},
		},
		{
			Summary: "summary-1",
			IssueId: "XRAY-1",
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{
				SeverityDetails:        formats.SeverityDetails{Severity: "high"},
				ImpactedDependencyName: "component-B",
			},
		},
		{
			Summary: "summary-2",
			IssueId: "XRAY-2",
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{
				SeverityDetails:        formats.SeverityDetails{Severity: "low"},
				ImpactedDependencyName: "component-C",
			},
		},
		{
			Summary: "summary-2",
			IssueId: "XRAY-2",
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{
				SeverityDetails:        formats.SeverityDetails{Severity: "low"},
				ImpactedDependencyName: "component-D",
			},
		},
	}

	// Run createAllIssuesRows and make sure that XRAY-1 and XRAY-2 vulnerabilities exists in the results
	vulnerabilities, licenses, err := getScanVulnerabilitiesRows(&audit.Results{ExtendedScanResults: &xrayutils.ExtendedScanResults{XrayResults: []services.ScanResponse{currentScan}}}, nil)
	assert.NoError(t, err)
	assert.Len(t, vulnerabilities, 4)
	assert.Len(t, licenses, 0)
	assert.ElementsMatch(t, expected, vulnerabilities)
}

func TestGetNewVulnerabilities(t *testing.T) {
	// Previous scan with only one vulnerability - XRAY-1
	previousScan := services.ScanResponse{
		Vulnerabilities: []services.Vulnerability{{
			IssueId:    "XRAY-1",
			Summary:    "summary-1",
			Severity:   "high",
			Cves:       []services.Cve{{Id: "CVE-2023-1234"}},
			Components: map[string]services.Component{"component-A": {}, "component-B": {}},
			Technology: coreutils.Maven.String(),
		}},
	}

	// Current scan with 2 vulnerabilities - XRAY-1 and XRAY-2
	currentScan := services.ScanResponse{
		Vulnerabilities: []services.Vulnerability{
			{
				IssueId:    "XRAY-1",
				Summary:    "summary-1",
				Severity:   "high",
				Cves:       []services.Cve{{Id: "CVE-2023-1234"}},
				Components: map[string]services.Component{"component-A": {}, "component-B": {}},
				Technology: coreutils.Maven.String(),
			},
			{
				IssueId:    "XRAY-2",
				Summary:    "summary-2",
				Severity:   "low",
				Cves:       []services.Cve{{Id: "CVE-2023-4321"}},
				Components: map[string]services.Component{"component-C": {}, "component-D": {}},
				Technology: coreutils.Yarn.String(),
			},
		},
	}

	expected := []formats.VulnerabilityOrViolationRow{
		{
			Summary:    "summary-2",
			Applicable: "Applicable",
			IssueId:    "XRAY-2",
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{
				SeverityDetails:        formats.SeverityDetails{Severity: "low"},
				ImpactedDependencyName: "component-C",
			},
			Cves:       []formats.CveRow{{Id: "CVE-2023-4321", Applicability: &formats.Applicability{Status: "Applicable", Evidence: []formats.Evidence{{Location: formats.Location{File: "file1", StartLine: 1, StartColumn: 10}}}}}},
			Technology: coreutils.Yarn,
		},
		{
			Summary:    "summary-2",
			Applicable: "Applicable",
			IssueId:    "XRAY-2",
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{
				SeverityDetails:        formats.SeverityDetails{Severity: "low"},
				ImpactedDependencyName: "component-D",
			},
			Cves:       []formats.CveRow{{Id: "CVE-2023-4321", Applicability: &formats.Applicability{Status: "Applicable", Evidence: []formats.Evidence{{Location: formats.Location{File: "file1", StartLine: 1, StartColumn: 10}}}}}},
			Technology: coreutils.Yarn,
		},
	}

	// Run createNewIssuesRows and make sure that only the XRAY-2 vulnerability exists in the results
	vulnerabilities, licenses, err := createNewVulnerabilitiesRows(
		&audit.Results{
			ExtendedScanResults: &xrayutils.ExtendedScanResults{
				XrayResults:    []services.ScanResponse{previousScan},
				EntitledForJas: true,
				ApplicabilityScanResults: []*sarif.Run{sarif.NewRunWithInformationURI("", "").
					WithResults([]*sarif.Result{
						sarif.NewRuleResult("applic_CVE-2023-4321").
							WithLocations([]*sarif.Location{
								sarif.NewLocationWithPhysicalLocation(sarif.NewPhysicalLocation().
									WithArtifactLocation(sarif.NewArtifactLocation().
										WithUri("file1")).
									WithRegion(sarif.NewRegion().
										WithStartLine(1).
										WithStartColumn(10))),
							}),
					}),
				},
			},
		},
		&audit.Results{
			ExtendedScanResults: &xrayutils.ExtendedScanResults{
				XrayResults:    []services.ScanResponse{currentScan},
				EntitledForJas: true,
				ApplicabilityScanResults: []*sarif.Run{sarif.NewRunWithInformationURI("", "").
					WithResults([]*sarif.Result{
						sarif.NewRuleResult("applic_CVE-2023-4321").
							WithLocations([]*sarif.Location{sarif.NewLocationWithPhysicalLocation(sarif.NewPhysicalLocation().
								WithArtifactLocation(sarif.NewArtifactLocation().
									WithUri("file1")).
								WithRegion(sarif.NewRegion().
									WithStartLine(1).
									WithStartColumn(10))),
							}),
					}),
				},
			},
		},
		nil,
	)
	assert.NoError(t, err)
	assert.Len(t, vulnerabilities, 2)
	assert.Len(t, licenses, 0)
	assert.ElementsMatch(t, expected, vulnerabilities)
}

func TestGetNewVulnerabilitiesCaseNoPrevVulnerabilities(t *testing.T) {
	// Previous scan with no vulnerabilities
	previousScan := services.ScanResponse{
		Vulnerabilities: []services.Vulnerability{},
	}

	// Current scan with 2 vulnerabilities - XRAY-1 and XRAY-2
	currentScan := services.ScanResponse{
		Vulnerabilities: []services.Vulnerability{
			{
				IssueId:             "XRAY-1",
				Summary:             "summary-1",
				Severity:            "high",
				ExtendedInformation: &services.ExtendedInformation{FullDescription: "description-1"},
				Components:          map[string]services.Component{"component-A": {}},
			},
			{
				IssueId:             "XRAY-2",
				Summary:             "summary-2",
				Severity:            "low",
				ExtendedInformation: &services.ExtendedInformation{FullDescription: "description-2"},
				Components:          map[string]services.Component{"component-B": {}},
			},
		},
	}

	expected := []formats.VulnerabilityOrViolationRow{
		{
			Summary: "summary-2",
			IssueId: "XRAY-2",
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{
				SeverityDetails:        formats.SeverityDetails{Severity: "low"},
				ImpactedDependencyName: "component-B",
			},
			JfrogResearchInformation: &formats.JfrogResearchInformation{Details: "description-2"},
		},
		{
			Summary: "summary-1",
			IssueId: "XRAY-1",
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{
				SeverityDetails:        formats.SeverityDetails{Severity: "high"},
				ImpactedDependencyName: "component-A",
			},
			JfrogResearchInformation: &formats.JfrogResearchInformation{Details: "description-1"},
		},
	}

	// Run createNewIssuesRows and expect both XRAY-1 and XRAY-2 vulnerability in the results
	vulnerabilities, licenses, err := createNewVulnerabilitiesRows(
		&audit.Results{ExtendedScanResults: &xrayutils.ExtendedScanResults{XrayResults: []services.ScanResponse{previousScan}}},
		&audit.Results{ExtendedScanResults: &xrayutils.ExtendedScanResults{XrayResults: []services.ScanResponse{currentScan}}},
		nil,
	)
	assert.NoError(t, err)
	assert.Len(t, vulnerabilities, 2)
	assert.Len(t, licenses, 0)
	assert.ElementsMatch(t, expected, vulnerabilities)
}

func TestGetNewVulnerabilitiesCaseNoNewVulnerabilities(t *testing.T) {
	// Previous scan with 2 vulnerabilities - XRAY-1 and XRAY-2
	previousScan := services.ScanResponse{
		Vulnerabilities: []services.Vulnerability{
			{
				IssueId:    "XRAY-1",
				Summary:    "summary-1",
				Severity:   "high",
				Components: map[string]services.Component{"component-A": {}},
			},
			{
				IssueId:    "XRAY-2",
				Summary:    "summary-2",
				Severity:   "low",
				Components: map[string]services.Component{"component-B": {}},
			},
		},
	}

	// Current scan with no vulnerabilities
	currentScan := services.ScanResponse{
		Vulnerabilities: []services.Vulnerability{},
	}

	// Run createNewIssuesRows and expect no vulnerability in the results
	vulnerabilities, licenses, err := createNewVulnerabilitiesRows(
		&audit.Results{ExtendedScanResults: &xrayutils.ExtendedScanResults{XrayResults: []services.ScanResponse{previousScan}}},
		&audit.Results{ExtendedScanResults: &xrayutils.ExtendedScanResults{XrayResults: []services.ScanResponse{currentScan}}},
		nil,
	)
	assert.NoError(t, err)
	assert.Len(t, vulnerabilities, 0)
	assert.Len(t, licenses, 0)
}

func TestCreatePullRequestMessageNoVulnerabilities(t *testing.T) {
	vulnerabilities := []formats.VulnerabilityOrViolationRow{}
	message := createPullRequestComment(&utils.IssuesCollection{Vulnerabilities: vulnerabilities}, &outputwriter.StandardOutput{})

	expectedMessageByte, err := os.ReadFile(filepath.Join("..", "testdata", "messages", "novulnerabilities.md"))
	assert.NoError(t, err)
	expectedMessage := strings.ReplaceAll(string(expectedMessageByte), "\r\n", "\n")
	assert.Equal(t, expectedMessage, message)

	outputWriter := &outputwriter.StandardOutput{}
	outputWriter.SetVcsProvider(vcsutils.GitLab)
	message = createPullRequestComment(&utils.IssuesCollection{Vulnerabilities: vulnerabilities}, outputWriter)

	expectedMessageByte, err = os.ReadFile(filepath.Join("..", "testdata", "messages", "novulnerabilitiesMR.md"))
	assert.NoError(t, err)
	expectedMessage = strings.ReplaceAll(string(expectedMessageByte), "\r\n", "\n")
	assert.Equal(t, expectedMessage, message)
}

func TestGetAllIssues(t *testing.T) {
	allowedLicenses := []string{"MIT"}
	auditResults := &audit.Results{
		ExtendedScanResults: &xrayutils.ExtendedScanResults{
			XrayResults: []services.ScanResponse{{
				Vulnerabilities: []services.Vulnerability{
					{Cves: []services.Cve{{Id: "CVE-2022-2122"}}, Severity: "High", Components: map[string]services.Component{"Dep-1": {FixedVersions: []string{"1.2.3"}}}},
					{Cves: []services.Cve{{Id: "CVE-2023-3122"}}, Severity: "Low", Components: map[string]services.Component{"Dep-2": {FixedVersions: []string{"1.2.2"}}}},
				},
				Licenses: []services.License{{Key: "Apache-2.0", Components: map[string]services.Component{"Dep-1": {FixedVersions: []string{"1.2.3"}}}}},
			}},
			ApplicabilityScanResults: []*sarif.Run{
				utils.GetRunWithDummyResults(
					utils.GetDummyResultWithOneLocation("file", 0, 0, "", "applic_CVE-2022-2122", ""),
					utils.GetDummyPassingResult("applic_CVE-2023-3122")),
			},
			SecretsScanResults: []*sarif.Run{
				utils.GetRunWithDummyResults(
					utils.GetDummyResultWithOneLocation("index.js", 2, 13, "access token exposed", "", ""),
				),
			},
			EntitledForJas: true,
		},
	}
	issuesRows, err := getAllIssues(auditResults, allowedLicenses)
	assert.NoError(t, err)
	assert.Len(t, issuesRows.Licenses, 1)
	assert.Len(t, issuesRows.Vulnerabilities, 2)
	assert.Len(t, issuesRows.Secrets, 1)
	assert.Equal(t, auditResults.ExtendedScanResults.XrayResults[0].Licenses[0].Key, "Apache-2.0")
	assert.Equal(t, "Dep-1", issuesRows.Licenses[0].ImpactedDependencyName)
	vuln1 := auditResults.ExtendedScanResults.XrayResults[0].Vulnerabilities[0]
	assert.Equal(t, vuln1.Cves[0].Id, issuesRows.Vulnerabilities[0].Cves[0].Id)
	assert.Equal(t, vuln1.Severity, issuesRows.Vulnerabilities[0].Severity)
	assert.Equal(t, vuln1.Components["Dep-1"].FixedVersions[0], issuesRows.Vulnerabilities[0].FixedVersions[0])
	vuln2 := auditResults.ExtendedScanResults.XrayResults[0].Vulnerabilities[1]
	assert.Equal(t, vuln2.Cves[0].Id, issuesRows.Vulnerabilities[1].Cves[0].Id)
	assert.Equal(t, vuln2.Severity, issuesRows.Vulnerabilities[1].Severity)
	assert.Equal(t, vuln2.Components["Dep-2"].FixedVersions[0], issuesRows.Vulnerabilities[1].FixedVersions[0])
	assert.Equal(t, auditResults.ExtendedScanResults.XrayResults[0].Licenses[0].Key, issuesRows.Licenses[0].LicenseKey)
	assert.Equal(t, "Dep-1", issuesRows.Licenses[0].ImpactedDependencyName)
	assert.Equal(t, xrayutils.GetResultSeverity(auditResults.ExtendedScanResults.SecretsScanResults[0].Results[0]), issuesRows.Secrets[0].Severity)
	assert.Equal(t, xrayutils.GetLocationFileName(auditResults.ExtendedScanResults.SecretsScanResults[0].Results[0].Locations[0]), issuesRows.Secrets[0].File)
	assert.Equal(t, *auditResults.ExtendedScanResults.SecretsScanResults[0].Results[0].Locations[0].PhysicalLocation.Region.Snippet.Text, issuesRows.Secrets[0].Snippet)
}

func TestCreatePullRequestComment(t *testing.T) {
	vulnerabilities := []formats.VulnerabilityOrViolationRow{
		{
			Summary: "Summary XRAY-122345",
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{
				SeverityDetails:           formats.SeverityDetails{Severity: "High"},
				ImpactedDependencyName:    "github.com/nats-io/nats-streaming-server",
				ImpactedDependencyVersion: "v0.21.0",
				Components: []formats.ComponentRow{
					{
						Name:    "github.com/nats-io/nats-streaming-server",
						Version: "v0.21.0",
					},
				},
			},
			Applicable:    "Undetermined",
			FixedVersions: []string{"[0.24.1]"},
			IssueId:       "XRAY-122345",
			Cves:          []formats.CveRow{{}},
		},
		{
			Summary: "Summary",
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{
				SeverityDetails:           formats.SeverityDetails{Severity: "High"},
				ImpactedDependencyName:    "github.com/mholt/archiver/v3",
				ImpactedDependencyVersion: "v3.5.1",
				Components: []formats.ComponentRow{
					{
						Name:    "github.com/mholt/archiver/v3",
						Version: "v3.5.1",
					},
				},
			},
			Applicable: "Undetermined",
			Cves:       []formats.CveRow{},
		},
		{
			Summary: "Summary CVE-2022-26652",
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{
				SeverityDetails:           formats.SeverityDetails{Severity: "Medium"},
				ImpactedDependencyName:    "github.com/nats-io/nats-streaming-server",
				ImpactedDependencyVersion: "v0.21.0",
				Components: []formats.ComponentRow{
					{
						Name:    "github.com/nats-io/nats-streaming-server",
						Version: "v0.21.0",
					},
				},
			},
			Applicable:    "Undetermined",
			FixedVersions: []string{"[0.24.3]"},
			Cves:          []formats.CveRow{{Id: "CVE-2022-26652"}},
		},
	}
	licenses := []formats.LicenseRow{
		{
			LicenseKey: "Apache-2.0",
			ImpactedDependencyDetails: formats.ImpactedDependencyDetails{
				SeverityDetails:           formats.SeverityDetails{Severity: "High", SeverityNumValue: 13},
				ImpactedDependencyName:    "minimatch",
				ImpactedDependencyVersion: "1.2.3",
				Components: []formats.ComponentRow{
					{
						Name:    "root",
						Version: "1.0.0",
					},
					{
						Name:    "minimatch",
						Version: "1.2.3",
					},
				},
			},
		},
	}

	writerOutput := &outputwriter.StandardOutput{}
	writerOutput.SetJasOutputFlags(true, true)
	message := createPullRequestComment(&utils.IssuesCollection{Vulnerabilities: vulnerabilities, Licenses: licenses}, writerOutput)

	expectedMessage := "<div align='center'>\n\n[![](https://raw.githubusercontent.com/jfrog/frogbot/master/resources/v2/vulnerabilitiesBannerPR.png)](https://github.com/jfrog/frogbot#readme)\n\n</div>\n\n\n## 📦 Vulnerable Dependencies\n\n### ✍️ Summary\n\n<div align=\"center\">\n\n| SEVERITY                | CONTEXTUAL ANALYSIS                  | DIRECT DEPENDENCIES                  | IMPACTED DEPENDENCY                   | FIXED VERSIONS                       | CVES                       |\n| :---------------------: | :----------------------------------: | :----------------------------------: | :-----------------------------------: | :---------------------------------: | :---------------------------------: | \n| ![](https://raw.githubusercontent.com/jfrog/frogbot/master/resources/v2/applicableHighSeverity.png)<br>    High | Undetermined | github.com/nats-io/nats-streaming-server:v0.21.0 | github.com/nats-io/nats-streaming-server:v0.21.0 | [0.24.1] |  -  |\n| ![](https://raw.githubusercontent.com/jfrog/frogbot/master/resources/v2/applicableHighSeverity.png)<br>    High | Undetermined | github.com/mholt/archiver/v3:v3.5.1 | github.com/mholt/archiver/v3:v3.5.1 |  -  |  -  |\n| ![](https://raw.githubusercontent.com/jfrog/frogbot/master/resources/v2/applicableMediumSeverity.png)<br>  Medium | Undetermined | github.com/nats-io/nats-streaming-server:v0.21.0 | github.com/nats-io/nats-streaming-server:v0.21.0 | [0.24.3] | CVE-2022-26652 |\n\n</div>\n\n## 🔬 Research Details\n\n<details>\n<summary> <b>[ XRAY-122345 ] github.com/nats-io/nats-streaming-server v0.21.0</b> </summary>\n<br>\n\n**Description:**\nSummary XRAY-122345\n\n\n</details>\n\n\n<details>\n<summary> <b>github.com/mholt/archiver/v3 v3.5.1</b> </summary>\n<br>\n\n**Description:**\nSummary\n\n\n</details>\n\n\n<details>\n<summary> <b>[ CVE-2022-26652 ] github.com/nats-io/nats-streaming-server v0.21.0</b> </summary>\n<br>\n\n**Description:**\nSummary CVE-2022-26652\n\n\n</details>\n\n\n## ⚖️ Violated Licenses \n\n<div align=\"center\">\n\n\n| LICENSE                | DIRECT DEPENDENCIES                  | IMPACTED DEPENDENCY                   | \n| :---------------------: | :----------------------------------: | :-----------------------------------: | \n| Apache-2.0 | root 1.0.0<br>minimatch 1.2.3 | minimatch 1.2.3 |\n\n</div>\n\n\n---\n<div align=\"center\">\n\n[🐸 JFrog Frogbot](https://github.com/jfrog/frogbot#readme)\n\n</div>"
	assert.Equal(t, expectedMessage, message)

	writerOutput.SetVcsProvider(vcsutils.GitLab)
	message = createPullRequestComment(&utils.IssuesCollection{Vulnerabilities: vulnerabilities}, writerOutput)
	expectedMessage = "<div align='center'>\n\n[![](https://raw.githubusercontent.com/jfrog/frogbot/master/resources/v2/vulnerabilitiesBannerMR.png)](https://github.com/jfrog/frogbot#readme)\n\n</div>\n\n\n## 📦 Vulnerable Dependencies\n\n### ✍️ Summary\n\n<div align=\"center\">\n\n| SEVERITY                | CONTEXTUAL ANALYSIS                  | DIRECT DEPENDENCIES                  | IMPACTED DEPENDENCY                   | FIXED VERSIONS                       | CVES                       |\n| :---------------------: | :----------------------------------: | :----------------------------------: | :-----------------------------------: | :---------------------------------: | :---------------------------------: | \n| ![](https://raw.githubusercontent.com/jfrog/frogbot/master/resources/v2/applicableHighSeverity.png)<br>    High | Undetermined | github.com/nats-io/nats-streaming-server:v0.21.0 | github.com/nats-io/nats-streaming-server:v0.21.0 | [0.24.1] |  -  |\n| ![](https://raw.githubusercontent.com/jfrog/frogbot/master/resources/v2/applicableHighSeverity.png)<br>    High | Undetermined | github.com/mholt/archiver/v3:v3.5.1 | github.com/mholt/archiver/v3:v3.5.1 |  -  |  -  |\n| ![](https://raw.githubusercontent.com/jfrog/frogbot/master/resources/v2/applicableMediumSeverity.png)<br>  Medium | Undetermined | github.com/nats-io/nats-streaming-server:v0.21.0 | github.com/nats-io/nats-streaming-server:v0.21.0 | [0.24.3] | CVE-2022-26652 |\n\n</div>\n\n## 🔬 Research Details\n\n<details>\n<summary> <b>[ XRAY-122345 ] github.com/nats-io/nats-streaming-server v0.21.0</b> </summary>\n<br>\n\n**Description:**\nSummary XRAY-122345\n\n\n</details>\n\n\n<details>\n<summary> <b>github.com/mholt/archiver/v3 v3.5.1</b> </summary>\n<br>\n\n**Description:**\nSummary\n\n\n</details>\n\n\n<details>\n<summary> <b>[ CVE-2022-26652 ] github.com/nats-io/nats-streaming-server v0.21.0</b> </summary>\n<br>\n\n**Description:**\nSummary CVE-2022-26652\n\n\n</details>\n\n\n---\n<div align=\"center\">\n\n[🐸 JFrog Frogbot](https://github.com/jfrog/frogbot#readme)\n\n</div>"
	assert.Equal(t, expectedMessage, message)
}

func TestScanPullRequest(t *testing.T) {
	tests := []struct {
		testName             string
		configPath           string
		projectName          string
		failOnSecurityIssues bool
	}{
		{
			testName:             "ScanPullRequest",
			configPath:           testProjConfigPath,
			projectName:          "test-proj",
			failOnSecurityIssues: true,
		},
		{
			testName:             "ScanPullRequestNoFail",
			configPath:           testProjConfigPathNoFail,
			projectName:          "test-proj",
			failOnSecurityIssues: false,
		},
		{
			testName:             "ScanPullRequestSubdir",
			configPath:           testProjSubdirConfigPath,
			projectName:          "test-proj-subdir",
			failOnSecurityIssues: true,
		},
		{
			testName:             "ScanPullRequestNoIssues",
			configPath:           testCleanProjConfigPath,
			projectName:          "clean-test-proj",
			failOnSecurityIssues: false,
		},
		{
			testName:             "ScanPullRequestMultiWorkDir",
			configPath:           testMultiDirProjConfigPathNoFail,
			projectName:          "multi-dir-test-proj",
			failOnSecurityIssues: false,
		},
		{
			testName:             "ScanPullRequestMultiWorkDirNoFail",
			configPath:           testMultiDirProjConfigPath,
			projectName:          "multi-dir-test-proj",
			failOnSecurityIssues: true,
		},
	}
	for _, test := range tests {
		t.Run(test.testName, func(t *testing.T) {
			testScanPullRequest(t, test.configPath, test.projectName, test.failOnSecurityIssues)
		})
	}
}

func testScanPullRequest(t *testing.T, configPath, projectName string, failOnSecurityIssues bool) {
	params, restoreEnv := utils.VerifyEnv(t)
	defer restoreEnv()

	// Create mock GitLab server
	server := httptest.NewServer(createGitLabHandler(t, projectName))
	defer server.Close()

	configAggregator, client := prepareConfigAndClient(t, configPath, server, params)
	testDir, cleanUp := utils.PrepareTestEnvironment(t, "scanpullrequest")
	defer cleanUp()

	// Renames test git folder to .git
	currentDir := filepath.Join(testDir, projectName)
	restoreDir, err := utils.Chdir(currentDir)
	assert.NoError(t, err)
	defer func() {
		assert.NoError(t, restoreDir())
		assert.NoError(t, fileutils.RemoveTempDir(currentDir))
	}()

	// Run "frogbot scan pull request"
	var scanPullRequest ScanPullRequestCmd
	err = scanPullRequest.Run(configAggregator, client)
	if failOnSecurityIssues {
		assert.EqualErrorf(t, err, securityIssueFoundErr, "Error should be: %v, got: %v", securityIssueFoundErr, err)
	} else {
		assert.NoError(t, err)
	}

	// Check env sanitize
	err = utils.SanitizeEnv()
	assert.NoError(t, err)
	utils.AssertSanitizedEnv(t)
}

func TestVerifyGitHubFrogbotEnvironment(t *testing.T) {
	// Init mock
	client := CreateMockVcsClient(t)
	environment := "frogbot"
	client.EXPECT().GetRepositoryInfo(context.Background(), gitParams.RepoOwner, gitParams.RepoName).Return(vcsclient.RepositoryInfo{}, nil)
	client.EXPECT().GetRepositoryEnvironmentInfo(context.Background(), gitParams.RepoOwner, gitParams.RepoName, environment).Return(vcsclient.RepositoryEnvironmentInfo{Reviewers: []string{"froggy"}}, nil)
	assert.NoError(t, os.Setenv(utils.GitHubActionsEnv, "true"))

	// Run verifyGitHubFrogbotEnvironment
	err := verifyGitHubFrogbotEnvironment(client, gitParams)
	assert.NoError(t, err)
}

func TestVerifyGitHubFrogbotEnvironmentNoEnv(t *testing.T) {
	// Redirect log to avoid negative output
	previousLogger := redirectLogOutputToNil()
	defer log.SetLogger(previousLogger)

	// Init mock
	client := CreateMockVcsClient(t)
	environment := "frogbot"
	client.EXPECT().GetRepositoryInfo(context.Background(), gitParams.RepoOwner, gitParams.RepoName).Return(vcsclient.RepositoryInfo{}, nil)
	client.EXPECT().GetRepositoryEnvironmentInfo(context.Background(), gitParams.RepoOwner, gitParams.RepoName, environment).Return(vcsclient.RepositoryEnvironmentInfo{}, errors.New("404"))
	assert.NoError(t, os.Setenv(utils.GitHubActionsEnv, "true"))

	// Run verifyGitHubFrogbotEnvironment
	err := verifyGitHubFrogbotEnvironment(client, gitParams)
	assert.ErrorContains(t, err, noGitHubEnvErr)
}

func TestVerifyGitHubFrogbotEnvironmentNoReviewers(t *testing.T) {
	// Init mock
	client := CreateMockVcsClient(t)
	environment := "frogbot"
	client.EXPECT().GetRepositoryInfo(context.Background(), gitParams.RepoOwner, gitParams.RepoName).Return(vcsclient.RepositoryInfo{}, nil)
	client.EXPECT().GetRepositoryEnvironmentInfo(context.Background(), gitParams.RepoOwner, gitParams.RepoName, environment).Return(vcsclient.RepositoryEnvironmentInfo{}, nil)
	assert.NoError(t, os.Setenv(utils.GitHubActionsEnv, "true"))

	// Run verifyGitHubFrogbotEnvironment
	err := verifyGitHubFrogbotEnvironment(client, gitParams)
	assert.ErrorContains(t, err, noGitHubEnvReviewersErr)
}

func TestVerifyGitHubFrogbotEnvironmentOnPrem(t *testing.T) {
	repoConfig := &utils.Repository{
		Params: utils.Params{Git: utils.Git{
			VcsInfo: vcsclient.VcsInfo{APIEndpoint: "https://acme.vcs.io"}},
		},
	}

	// Run verifyGitHubFrogbotEnvironment
	err := verifyGitHubFrogbotEnvironment(&vcsclient.GitHubClient{}, repoConfig)
	assert.NoError(t, err)
}

func prepareConfigAndClient(t *testing.T, configPath string, server *httptest.Server, serverParams coreconfig.ServerDetails) (utils.RepoAggregator, vcsclient.VcsClient) {
	gitTestParams := &utils.Git{
		GitProvider: vcsutils.GitHub,
		RepoOwner:   "jfrog",
		VcsInfo: vcsclient.VcsInfo{
			Token:       "123456",
			APIEndpoint: server.URL,
		},
		PullRequestDetails: vcsclient.PullRequestInfo{ID: int64(1)},
	}
	utils.SetEnvAndAssert(t, map[string]string{utils.GitPullRequestIDEnv: "1"})

	configData, err := utils.ReadConfigFromFileSystem(configPath)
	assert.NoError(t, err)
	configAggregator, err := utils.BuildRepoAggregator(configData, gitTestParams, &serverParams, utils.ScanPullRequest)
	assert.NoError(t, err)

	client, err := vcsclient.NewClientBuilder(vcsutils.GitLab).ApiEndpoint(server.URL).Token("123456").Build()
	assert.NoError(t, err)
	return configAggregator, client
}

// Create HTTP handler to mock GitLab server
func createGitLabHandler(t *testing.T, projectName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		// Return 200 on ping
		case r.RequestURI == "/api/v4/":
			w.WriteHeader(http.StatusOK)
		// Mimic get pull request by ID
		case r.RequestURI == fmt.Sprintf("/api/v4/projects/jfrog%s/merge_requests/1", "%2F"+projectName):
			w.WriteHeader(http.StatusOK)
			expectedResponse, err := os.ReadFile(filepath.Join("..", "expectedPullRequestDetailsResponse.json"))
			assert.NoError(t, err)
			_, err = w.Write(expectedResponse)
			assert.NoError(t, err)
		// Mimic download specific branch to scan
		case r.RequestURI == fmt.Sprintf("/api/v4/projects/jfrog%s/repository/archive.tar.gz?sha=%s", "%2F"+projectName, testSourceBranchName):
			w.WriteHeader(http.StatusOK)
			repoFile, err := os.ReadFile(filepath.Join("..", projectName, "sourceBranch.gz"))
			assert.NoError(t, err)
			_, err = w.Write(repoFile)
			assert.NoError(t, err)
		// Download repository mock
		case r.RequestURI == fmt.Sprintf("/api/v4/projects/jfrog%s/repository/archive.tar.gz?sha=%s", "%2F"+projectName, testTargetBranchName):
			w.WriteHeader(http.StatusOK)
			repoFile, err := os.ReadFile(filepath.Join("..", projectName, "targetBranch.gz"))
			assert.NoError(t, err)
			_, err = w.Write(repoFile)
			assert.NoError(t, err)
			return
		// clean-test-proj should not include any vulnerabilities so assertion is not needed.
		case r.RequestURI == fmt.Sprintf("/api/v4/projects/jfrog%s/merge_requests/133/notes", "%2Fclean-test-proj") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)
			_, err := w.Write([]byte("{}"))
			assert.NoError(t, err)
			return
		case r.RequestURI == fmt.Sprintf("/api/v4/projects/jfrog%s/merge_requests/133/notes", "%2Fclean-test-proj") && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
			comments, err := os.ReadFile(filepath.Join("..", "commits.json"))
			assert.NoError(t, err)
			_, err = w.Write(comments)
			assert.NoError(t, err)
		// Return 200 when using the REST that creates the comment
		case r.RequestURI == fmt.Sprintf("/api/v4/projects/jfrog%s/merge_requests/133/notes", "%2F"+projectName) && r.Method == http.MethodPost:
			buf := new(bytes.Buffer)
			_, err := buf.ReadFrom(r.Body)
			assert.NoError(t, err)
			assert.NotEmpty(t, buf.String())

			var expectedResponse []byte
			switch {
			case strings.Contains(projectName, "multi-dir"):
				expectedResponse, err = os.ReadFile(filepath.Join("..", "expectedResponseMultiDir.json"))
			case strings.Contains(projectName, "pip"):
				expectedResponse, err = os.ReadFile(filepath.Join("..", "expectedResponsePip.json"))
			default:
				expectedResponse, err = os.ReadFile(filepath.Join("..", "expectedResponse.json"))
			}
			assert.NoError(t, err)
			assert.JSONEq(t, string(expectedResponse), buf.String())

			w.WriteHeader(http.StatusOK)
			_, err = w.Write([]byte("{}"))
			assert.NoError(t, err)
		case r.RequestURI == fmt.Sprintf("/api/v4/projects/jfrog%s/merge_requests/133/notes", "%2F"+projectName) && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
			comments, err := os.ReadFile(filepath.Join("..", "commits.json"))
			assert.NoError(t, err)
			_, err = w.Write(comments)
			assert.NoError(t, err)
		case r.RequestURI == fmt.Sprintf("/api/v4/projects/jfrog%s", "%2F"+projectName):
			jsonResponse := `{"id": 3,"visibility": "private","ssh_url_to_repo": "git@example.com:diaspora/diaspora-project-site.git","http_url_to_repo": "https://example.com/diaspora/diaspora-project-site.git"}`
			_, err := w.Write([]byte(jsonResponse))
			assert.NoError(t, err)
		case r.RequestURI == fmt.Sprintf("/api/v4/projects/jfrog%s/merge_requests/133/discussions", "%2F"+projectName):
			discussions, err := os.ReadFile(filepath.Join("..", "list_merge_request_discussion_items.json"))
			assert.NoError(t, err)
			_, err = w.Write(discussions)
			assert.NoError(t, err)
		}
	}
}

func TestCreateNewIacRows(t *testing.T) {
	testCases := []struct {
		name                            string
		targetIacResults                []*sarif.Result
		sourceIacResults                []*sarif.Result
		expectedAddedIacVulnerabilities []formats.SourceCodeRow
	}{
		{
			name: "No vulnerabilities in source IaC results",
			targetIacResults: []*sarif.Result{
				sarif.NewRuleResult("").WithLevel(xrayutils.ConvertToSarifLevel("high")).WithLocations([]*sarif.Location{
					sarif.NewLocationWithPhysicalLocation(sarif.NewPhysicalLocation().
						WithArtifactLocation(sarif.NewArtifactLocation().WithUri("file1")).
						WithRegion(sarif.NewRegion().WithStartLine(1).WithStartColumn(10).WithSnippet(sarif.NewArtifactContent().WithText("aws violation"))),
					),
				}),
			},
			sourceIacResults:                []*sarif.Result{},
			expectedAddedIacVulnerabilities: []formats.SourceCodeRow{},
		},
		{
			name:             "No vulnerabilities in target IaC results",
			targetIacResults: []*sarif.Result{},
			sourceIacResults: []*sarif.Result{
				sarif.NewRuleResult("").WithLevel(xrayutils.ConvertToSarifLevel("high")).WithLocations([]*sarif.Location{
					sarif.NewLocationWithPhysicalLocation(sarif.NewPhysicalLocation().
						WithArtifactLocation(sarif.NewArtifactLocation().WithUri("file1")).
						WithRegion(sarif.NewRegion().WithStartLine(1).WithStartColumn(10).WithSnippet(sarif.NewArtifactContent().WithText("aws violation"))),
					),
				}),
			},
			expectedAddedIacVulnerabilities: []formats.SourceCodeRow{
				{
					SeverityDetails: formats.SeverityDetails{
						Severity:         "High",
						SeverityNumValue: 13,
					},
					Location: formats.Location{
						File:        "file1",
						StartLine:   1,
						StartColumn: 10,
						Snippet:     "aws violation",
					},
				},
			},
		},
		{
			name: "Some new vulnerabilities in source IaC results",
			targetIacResults: []*sarif.Result{
				sarif.NewRuleResult("").WithLevel(xrayutils.ConvertToSarifLevel("high")).WithLocations([]*sarif.Location{
					sarif.NewLocationWithPhysicalLocation(sarif.NewPhysicalLocation().
						WithArtifactLocation(sarif.NewArtifactLocation().WithUri("file1")).
						WithRegion(sarif.NewRegion().WithStartLine(1).WithStartColumn(10).WithSnippet(sarif.NewArtifactContent().WithText("aws violation"))),
					),
				}),
			},
			sourceIacResults: []*sarif.Result{
				sarif.NewRuleResult("").WithLevel(xrayutils.ConvertToSarifLevel("medium")).WithLocations([]*sarif.Location{
					sarif.NewLocationWithPhysicalLocation(sarif.NewPhysicalLocation().
						WithArtifactLocation(sarif.NewArtifactLocation().WithUri("file2")).
						WithRegion(sarif.NewRegion().WithStartLine(2).WithStartColumn(5).WithSnippet(sarif.NewArtifactContent().WithText("gcp violation"))),
					),
				}),
			},
			expectedAddedIacVulnerabilities: []formats.SourceCodeRow{
				{
					SeverityDetails: formats.SeverityDetails{
						Severity:         "Medium",
						SeverityNumValue: 11,
					},
					Location: formats.Location{
						File:        "file2",
						StartLine:   2,
						StartColumn: 5,
						Snippet:     "gcp violation",
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			targetIacRows := xrayutils.PrepareIacs([]*sarif.Run{sarif.NewRunWithInformationURI("", "").WithResults(tc.targetIacResults)})
			sourceIacRows := xrayutils.PrepareIacs([]*sarif.Run{sarif.NewRunWithInformationURI("", "").WithResults(tc.sourceIacResults)})
			addedIacVulnerabilities := createNewSourceCodeRows(targetIacRows, sourceIacRows)
			assert.ElementsMatch(t, tc.expectedAddedIacVulnerabilities, addedIacVulnerabilities)
		})
	}
}

func TestCreateNewSecretRows(t *testing.T) {
	testCases := []struct {
		name                                string
		targetSecretsResults                []*sarif.Result
		sourceSecretsResults                []*sarif.Result
		expectedAddedSecretsVulnerabilities []formats.SourceCodeRow
	}{
		{
			name: "No vulnerabilities in source secrets results",
			targetSecretsResults: []*sarif.Result{
				sarif.NewRuleResult("").WithMessage(sarif.NewTextMessage("Secret")).WithLevel(xrayutils.ConvertToSarifLevel("high")).WithLocations([]*sarif.Location{
					sarif.NewLocationWithPhysicalLocation(sarif.NewPhysicalLocation().
						WithArtifactLocation(sarif.NewArtifactLocation().WithUri("file1")).
						WithRegion(sarif.NewRegion().WithStartLine(1).WithStartColumn(10).WithSnippet(sarif.NewArtifactContent().WithText("Sensitive information"))),
					),
				}),
			},
			sourceSecretsResults:                []*sarif.Result{},
			expectedAddedSecretsVulnerabilities: []formats.SourceCodeRow{},
		},
		{
			name:                 "No vulnerabilities in target secrets results",
			targetSecretsResults: []*sarif.Result{},
			sourceSecretsResults: []*sarif.Result{
				sarif.NewRuleResult("").WithMessage(sarif.NewTextMessage("Secret")).WithLevel(xrayutils.ConvertToSarifLevel("high")).WithLocations([]*sarif.Location{
					sarif.NewLocationWithPhysicalLocation(sarif.NewPhysicalLocation().
						WithArtifactLocation(sarif.NewArtifactLocation().WithUri("file1")).
						WithRegion(sarif.NewRegion().WithStartLine(1).WithStartColumn(10).WithSnippet(sarif.NewArtifactContent().WithText("Sensitive information"))),
					),
				}),
			},
			expectedAddedSecretsVulnerabilities: []formats.SourceCodeRow{
				{
					SeverityDetails: formats.SeverityDetails{
						Severity:         "High",
						SeverityNumValue: 13,
					},
					Finding: "Secret",
					Location: formats.Location{
						File:        "file1",
						StartLine:   1,
						StartColumn: 10,
						Snippet:     "Sensitive information",
					},
				},
			},
		},
		{
			name: "Some new vulnerabilities in source secrets results",
			targetSecretsResults: []*sarif.Result{
				sarif.NewRuleResult("").WithMessage(sarif.NewTextMessage("Secret")).WithLevel(xrayutils.ConvertToSarifLevel("high")).WithLocations([]*sarif.Location{
					sarif.NewLocationWithPhysicalLocation(sarif.NewPhysicalLocation().
						WithArtifactLocation(sarif.NewArtifactLocation().WithUri("file1")).
						WithRegion(sarif.NewRegion().WithStartLine(1).WithStartColumn(10).WithSnippet(sarif.NewArtifactContent().WithText("Sensitive information"))),
					),
				}),
			},
			sourceSecretsResults: []*sarif.Result{
				sarif.NewRuleResult("").WithMessage(sarif.NewTextMessage("Secret")).WithLevel(xrayutils.ConvertToSarifLevel("medium")).WithLocations([]*sarif.Location{
					sarif.NewLocationWithPhysicalLocation(sarif.NewPhysicalLocation().
						WithArtifactLocation(sarif.NewArtifactLocation().WithUri("file2")).
						WithRegion(sarif.NewRegion().WithStartLine(2).WithStartColumn(5).WithSnippet(sarif.NewArtifactContent().WithText("Confidential data"))),
					),
				}),
			},
			expectedAddedSecretsVulnerabilities: []formats.SourceCodeRow{
				{
					SeverityDetails: formats.SeverityDetails{
						Severity:         "Medium",
						SeverityNumValue: 11,
					},
					Finding: "Secret",
					Location: formats.Location{
						File:        "file2",
						StartLine:   2,
						StartColumn: 5,
						Snippet:     "Confidential data",
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			targetSecretsRows := xrayutils.PrepareSecrets([]*sarif.Run{sarif.NewRunWithInformationURI("", "").WithResults(tc.targetSecretsResults)})
			sourceSecretsRows := xrayutils.PrepareSecrets([]*sarif.Run{sarif.NewRunWithInformationURI("", "").WithResults(tc.sourceSecretsResults)})
			addedSecretsVulnerabilities := createNewSourceCodeRows(targetSecretsRows, sourceSecretsRows)
			assert.ElementsMatch(t, tc.expectedAddedSecretsVulnerabilities, addedSecretsVulnerabilities)
		})
	}
}

func TestDeletePreviousPullRequestMessages(t *testing.T) {
	repository := &utils.Repository{
		Params: utils.Params{
			Git: utils.Git{
				PullRequestDetails: vcsclient.PullRequestInfo{Target: vcsclient.BranchInfo{
					Repository: "repo",
					Owner:      "owner",
				}, ID: 17},
			},
		},
		OutputWriter: &outputwriter.StandardOutput{},
	}
	client := CreateMockVcsClient(t)

	// Test with comment returned
	client.EXPECT().ListPullRequestComments(context.Background(), "owner", "repo", 17).Return([]vcsclient.CommentInfo{
		{ID: 20, Content: outputwriter.GetBanner(outputwriter.NoVulnerabilityPrBannerSource) + "text \n table\n text text text", Created: time.Unix(3, 0)},
	}, nil)
	client.EXPECT().DeletePullRequestComment(context.Background(), "owner", "repo", 17, 20).Return(nil).AnyTimes()
	err := deleteExistingPullRequestComment(repository, client)
	assert.NoError(t, err)

	// Test with no comment returned
	client.EXPECT().ListPullRequestComments(context.Background(), "owner", "repo", 17).Return([]vcsclient.CommentInfo{}, nil)
	err = deleteExistingPullRequestComment(repository, client)
	assert.NoError(t, err)

	// Test with error returned
	client.EXPECT().ListPullRequestComments(context.Background(), "owner", "repo", 17).Return(nil, errors.New("error"))
	err = deleteExistingPullRequestComment(repository, client)
	assert.Error(t, err)
}

// Set new logger with output redirection to a null logger. This is useful for negative tests.
// Caller is responsible to set the old log back.
func redirectLogOutputToNil() (previousLog log.Log) {
	previousLog = log.Logger
	newLog := log.NewLogger(log.ERROR, nil)
	newLog.SetOutputWriter(io.Discard)
	newLog.SetLogsWriter(io.Discard, 0)
	log.SetLogger(newLog)
	return previousLog
}
