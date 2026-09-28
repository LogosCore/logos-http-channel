module github.com/logoscore/logos-http-channel

go 1.25.7

require (
	github.com/joho/godotenv v1.5.1
	github.com/logoscore/logos-golang-protocol v0.1.0
)

require github.com/oklog/ulid/v2 v2.1.1 // indirect

replace github.com/logoscore/logos-golang-protocol => ../logos-golang-protocol
