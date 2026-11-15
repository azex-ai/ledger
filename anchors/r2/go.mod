module github.com/azex-ai/ledger/anchors/r2

go 1.26.6

require (
	github.com/aws/aws-sdk-go-v2 v1.48.0
	github.com/aws/aws-sdk-go-v2/credentials v1.21.0
	github.com/aws/aws-sdk-go-v2/service/s3 v1.114.10
	github.com/aws/smithy-go v1.28.9
	github.com/azex-ai/ledger v0.0.0-00010101000000-000000000000
	github.com/azex-ai/ledger/anchors/r2/internal/s3test v0.0.0-00010101000000-000000000000
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.27 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.12 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.12 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.25 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.12 // indirect
	github.com/johannesboyne/gofakes3 v1.2.0 // indirect
	github.com/ryszard/goskiplist v0.0.0-20150312221310-2dfbae5fcf46 // indirect
	github.com/shopspring/decimal v1.6.0 // indirect
	go.shabbyrobe.org/gocovmerge v0.0.0-20230507111327-fa4f82cfbf4d // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
)

replace github.com/azex-ai/ledger => ../..

// s3test is a test-only sibling module carrying the in-process fake S3
// server (gofakes3), kept out of this module's own require block so a
// consumer importing anchors/r2 does not pull it as a direct dependency
// (MJ-6, 2026-08-29 review; same split as internal/postgrestest).
replace github.com/azex-ai/ledger/anchors/r2/internal/s3test => ./internal/s3test
