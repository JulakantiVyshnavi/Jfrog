package packagehandlers

import (
	"errors"
	"github.com/jfrog/frogbot/utils"
)

type UnsupportedPackageHandler struct {
}

func (uph *UnsupportedPackageHandler) UpdateDependency(vulnDetails *utils.VulnerabilityDetails) error {
	return errors.New("frogbot currently does not support opening a pull request that fixes vulnerabilities in " + vulnDetails.Technology.ToFormal())
}
