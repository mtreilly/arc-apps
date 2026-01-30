module github.com/yourorg/arc-apps

go 1.23

require (
	github.com/dustin/go-humanize v1.0.1
	github.com/spf13/cobra v1.8.1
	github.com/yourorg/arc-sdk v0.1.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
)

replace github.com/yourorg/arc-sdk => ../arc-sdk
