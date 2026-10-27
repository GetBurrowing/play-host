package unreal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// EngineVersion reads <ue_root>/Engine/Build/Build.version and returns
// "major.minor.patch" (for example 5.4.2).
func EngineVersion(ueRoot string) (string, error) {
	b, err := os.ReadFile(filepath.Join(ueRoot, "Engine", "Build", "Build.version"))
	if err != nil {
		return "", err
	}
	var v struct {
		Major *int `json:"MajorVersion"`
		Minor *int `json:"MinorVersion"`
		Patch *int `json:"PatchVersion"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return "", fmt.Errorf("Build.version: %w", err)
	}
	if v.Major == nil || v.Minor == nil || v.Patch == nil {
		return "", fmt.Errorf("Build.version: missing a version field")
	}
	return fmt.Sprintf("%d.%d.%d", *v.Major, *v.Minor, *v.Patch), nil
}
