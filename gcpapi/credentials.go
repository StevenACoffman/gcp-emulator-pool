package gcpapi

import (
	"io/ioutil"
	"os"

	"github.com/Khan/districts-jobs/pkg/errors"
)

func NewCredentials(credFilePath string) ([]byte, error) {
	if credFilePath == "" {
		return []byte{}, nil
	}
	var credentials []byte
	if fileExists(credFilePath) {
		var fErr error
		credentials, fErr = ioutil.ReadFile(credFilePath)
		if fErr != nil {
			return []byte{}, errors.Newf(
				"could not read credentials from %s %w",
				credFilePath,
				fErr,
			)
		}
	} else {
		return []byte{}, errors.Newf("could not read credentials from %s", credFilePath)
	}
	return credentials, nil
}

// fileExists checks if a file exists and is not a directory before we
// try using it to prevent further errors.
func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}
