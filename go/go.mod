module github.com/moqom-cloud/moqom/go

go 1.23

// The SDK is its own repository, because `go get` resolves a module by fetching
// its path and a path is a URL. Until it has a published tag there is no
// version to resolve, so this points at a sibling checkout -- which is where it
// lands if you cloned both into the same directory. Drop this at the first
// release and name a real version above.
replace github.com/moqom-cloud/moqom-go => ../../moqom-go

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
