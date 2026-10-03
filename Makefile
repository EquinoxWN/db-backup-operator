.PHONY: setup generate verify-generated lint test envtest bench audit ci

CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1
SETUP_ENVTEST = go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.23
ENVTEST_K8S ?= 1.35.x

setup:
	go mod download

# Deepcopy code, CRD and RBAC manifests from the api/ types and kubebuilder markers.
generate:
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/v1alpha1
	$(CONTROLLER_GEN) rbac:roleName=db-backup-operator crd paths=./api/v1alpha1 paths=./internal/controller output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac

# Fails if api/ or config/ differ from what controller-gen produces (needs a git checkout).
verify-generated: generate
	git diff --exit-code -- api config

lint:
	@files="$$(gofmt -l $$(go list -f '{{$$d := .Dir}}{{range .GoFiles}}{{$$d}}/{{.}} {{end}}{{range .TestGoFiles}}{{$$d}}/{{.}} {{end}}' ./...))"; test -z "$$files" || (echo "$$files"; echo "run gofmt -w"; exit 1)
	go vet ./...

# Reconciler unit tests with a fake API client and a fake pod executor.
test:
	go test -race -count=1 ./...

# Same package against a real kube-apiserver and etcd (envtest). Linux, macOS or WSL: on Windows
# envtest cannot stop its child processes.
envtest:
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S) --bin-dir $(CURDIR)/.tmp/envtest -p path)" go test -count=1 -v -run 'APIServer|RealAPI' ./internal/controller

bench:
	@echo "M3: restore-drill history with measured restore time (RTO) and data loss (RPO)"

# Known vulnerabilities in the code paths actually called (needs Go 1.26+, as in CI).
audit:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

ci: setup lint verify-generated test envtest
