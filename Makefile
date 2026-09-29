BINARY := quick8s
CMD    := ./cmd/quick8s
COLIMA_PROFILE ?= quick8s-e2e

.PHONY: build test e2e e2e-clean vet fmt clean

build:
	go build -o bin/$(BINARY) $(CMD)

test:
	go test ./...

e2e: build
	./hack/e2e-test.sh

# e2e-test.sh tears down its own VM on exit, but a killed/crashed run (or one
# left up via KEEP_VM=1) can strand it - this destroys it unconditionally.
# -d also wipes its disk (see hack/e2e-test.sh for why that matters).
e2e-clean:
	colima delete -f -d --profile $(COLIMA_PROFILE)

vet:
	go vet ./...

fmt:
	gofmt -l .

clean:
	rm -rf bin
