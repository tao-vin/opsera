package version

import "runtime"

var (
	Version   = "2026.06.26.1"
	BuildTime = "dev"
	Commit    = "dev"
)

type Info struct {
	Product   string `json:"product"`
	Version   string `json:"version"`
	BuildTime string `json:"buildTime"`
	Commit    string `json:"commit"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
}

func Current() Info {
	return Info{
		Product:   "opsera",
		Version:   Version,
		BuildTime: BuildTime,
		Commit:    Commit,
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
	}
}
