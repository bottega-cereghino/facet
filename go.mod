module github.com/bottega-cereghino/facet

go 1.25.0

// The toolchain builds facet, here and for `go install …@version`, whatever Go
// is installed: 1.25.13 fixes GO-2026-6088 in encoding/xml, which reading a
// launchd plist reaches. Renovate moves it; `go` stays the oldest Go that works.
toolchain go1.25.13

require (
	github.com/spf13/cobra v1.8.1
	github.com/spf13/pflag v1.0.5
	golang.org/x/sys v0.47.0
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/inconshreveable/mousetrap v1.1.0 // indirect
