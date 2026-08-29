module github.com/simonjanss/rig

go 1.26.6

require (
	github.com/goccy/go-yaml v1.19.2
	github.com/google/go-cmp v0.7.0
	github.com/google/uuid v1.6.0
	github.com/invopop/jsonschema v0.14.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/pb33f/libopenapi v0.38.7
	github.com/pb33f/libopenapi-validator v0.14.0
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2
	github.com/spf13/cobra v1.10.2
	golang.org/x/text v0.40.0
)

require (
	github.com/basgys/goxml2json v1.1.1-0.20231018121955-e66ee54ceaad // indirect
	github.com/go-openapi/jsonpointer v0.23.2 // indirect
	github.com/go-openapi/swag/jsonname v0.26.1 // indirect
	github.com/pb33f/jsonpath v0.8.2 // indirect
	github.com/pressly/goose/v3 v3.27.3 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

require (
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	github.com/simonjanss/rig/auth v0.0.0
	github.com/simonjanss/rig/files v0.0.0
	github.com/simonjanss/rig/migrate v0.0.0
	github.com/simonjanss/rig/notify v0.0.0
	github.com/simonjanss/rig/presence v0.0.0
	github.com/simonjanss/rig/runtime v0.0.0
	github.com/spf13/pflag v1.0.9 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.6
	golang.org/x/sync v0.22.0 // indirect
)

replace github.com/simonjanss/rig/auth => ./auth

replace github.com/simonjanss/rig/migrate => ./migrate

replace github.com/simonjanss/rig/runtime => ./runtime

replace github.com/simonjanss/rig/files => ./files

replace github.com/simonjanss/rig/notify => ./notify

replace github.com/simonjanss/rig/presence => ./presence
