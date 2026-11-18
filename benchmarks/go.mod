module github.com/oarkflow/codec/benchmarks

go 1.27.3

require (
	github.com/fxamacker/cbor/v2 v2.10.0
	github.com/oarkflow/codec v0.0.0
	github.com/shamaton/msgpack/v2 v2.4.6
	github.com/vmihailenco/msgpack/v5 v5.4.1
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	github.com/x448/float16 v0.8.4 // indirect
)

replace github.com/oarkflow/codec => ../
