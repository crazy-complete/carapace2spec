CARA_BIN ?= carapace-bin
export CARA_BIN
CARA_BIN_REPO ?= https://github.com/carapace-sh/carapace-bin
CARA_BIN_REF ?= c7dbfd459fc1ead5b71b131e92763042c44a2d89

.PHONY: build test

build:
	go build -o carapace2spec .

test: $(CARA_BIN)
	go test -count=1 ./...

$(CARA_BIN):
	git clone --depth 1 $(CARA_BIN_REPO) $(CARA_BIN)
	git -C $(CARA_BIN) fetch --depth 1 origin $(CARA_BIN_REF)
	git -C $(CARA_BIN) checkout --detach $(CARA_BIN_REF)

clean:
	rm -rf $(CARA_BIN)
	rm -f carapace2spec
