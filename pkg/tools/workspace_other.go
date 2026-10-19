//go:build !unix

package tools

import (
	"fmt"
	"os"
)

func openWorkspaceRoot(string) (*os.File, error) {
	return nil, fmt.Errorf("secure workspace file access currently requires Unix")
}
func openWorkspaceFile(*os.File, string, bool, bool) (*os.File, error) {
	return nil, fmt.Errorf("secure workspace file access currently requires Unix")
}
