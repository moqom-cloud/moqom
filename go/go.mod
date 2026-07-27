module github.com/moqom-cloud/moqom/go

go 1.23

replace github.com/moqom-cloud/moqom-go => ../sdk/go

require (
	github.com/moqom-cloud/moqom-go v0.0.0-00010101000000-000000000000
	google.golang.org/grpc v1.71.0
	google.golang.org/protobuf v1.36.6
)

require (
	golang.org/x/net v0.34.0 // indirect
	golang.org/x/sys v0.29.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250115164207-1a7da9e5054f // indirect
)
