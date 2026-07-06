package main

import goutils "github.com/jkaninda/go-utils"

// version is the build version. Set it at build time with:
//
//	go build -ldflags "-X main.version=1.0.0"
//
// It is overridable at runtime with APP_VERSION — handy for the canary demo,
// where you run two otherwise-identical builds (e.g. 1.0.0 and 2.0.0) and watch
// which one serves each request.
var version = "dev"

// resolveVersion returns APP_VERSION when set, otherwise the build version.
func resolveVersion() string {
	return goutils.Env("APP_VERSION", version)
}
