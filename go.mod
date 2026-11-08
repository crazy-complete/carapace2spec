module github.com/crazy-complete/carapace2spec

go 1.26.0

require (
	github.com/carapace-sh/carapace v1.17.1
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/tools v0.52.0 // reader must support the export-data (pkgbits) version written by the Go in use (V5 for Go 1.27+), else go/packages dies with "internal error: package ... without types"
)

require (
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
)

replace github.com/spf13/pflag => github.com/carapace-sh/carapace-pflag v1.3.0
