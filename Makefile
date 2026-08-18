PLUGIN_ID := glm-vision-bridge
VERSION ?= 1.1.2
GO ?= go
DIST := dist
BINARY := $(DIST)/$(PLUGIN_ID).so
ARCHIVE := $(DIST)/$(PLUGIN_ID)_$(VERSION)_linux_amd64.zip
LDFLAGS := -X main.version=$(VERSION)

.PHONY: test race vet check build-linux-amd64 package-linux-amd64 clean

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

check: test vet race

build-linux-amd64:
	mkdir -p $(DIST)
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 $(GO) build -buildvcs=false -buildmode=c-shared -ldflags "$(LDFLAGS)" -o $(BINARY) .
	rm -f $(DIST)/$(PLUGIN_ID).h

package-linux-amd64: build-linux-amd64
	rm -f $(ARCHIVE) $(DIST)/checksums.txt
	zip -q -j $(ARCHIVE) $(BINARY) config.example.yaml LICENSE README.md
	cd $(DIST) && sha256sum $(notdir $(BINARY)) $(notdir $(ARCHIVE)) > checksums.txt

clean:
	rm -rf $(DIST)
