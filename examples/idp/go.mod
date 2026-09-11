module github.com/simonjanss/rig/examples/idp

go 1.26.6

replace github.com/simonjanss/rig/auth => ../../auth

replace github.com/simonjanss/rig/runtime => ../../runtime

require (
	github.com/simonjanss/rig/auth v0.0.0
	golang.org/x/oauth2 v0.36.0
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.10.0 // indirect
	github.com/simonjanss/rig/runtime v0.5.1 // indirect
	golang.org/x/text v0.40.0 // indirect
)
