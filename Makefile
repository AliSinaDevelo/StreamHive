.PHONY: build run test test-race test-fairness test-inventory-fairness test-eviction-repair test-inventory-consistency test-inventory-status test-peer-admission test-peer-drain test-tls-auth test-mtls test-mtls-cli test-tls-rotation test-tls-credential-health test-prometheus-format test-health-server test-cli-shutdown test-lifecycle-compaction test-lifecycle-membership test-lifecycle-compose test-fuzz test-budgets bench-inventory bench-inventory-wire vet cover lint demo-replication demo-compose demo-auth demo-repair demo-failure demo-continuation demo-inventory-budget demo-status ci help

build:
	@mkdir -p bin
	@go build -o bin/fs .

run: build
	@./bin/fs

test:
	@go test -count=1 ./...

test-race:
	@go test -race -count=1 ./...

test-fairness:
	@go test -race -count=20 -run '^TestRepairContinuationSchedulerKeepsPeersIndependent$$' ./...

test-inventory-fairness:
	@go test -race -count=5 -run '^TestRun_budgetedInventoryConvergesAcrossPeersAndSourceMutation$$' ./...

test-eviction-repair:
	@go test -race -count=5 -run '^TestRun_localEvictionRehydratesThroughStartupOnlyRepair$$' ./...

test-inventory-consistency:
	@go test -race -count=3 -run '^TestRun_periodicInventoryRepairsKeyAddedBehindLiveCursor$$' ./...

test-inventory-status:
	@go test -race -count=3 -run '^TestRun_inventoryStatusConvergesAfterAntiEntropy$$' ./...

test-peer-admission:
	@go test -race -count=3 -run '^TestRun_maxPeersRejectsSecondRealTCPPeer$$' ./...

test-peer-drain:
	@go test -race -count=3 -run '^(TestDrain_|TestClose_)' ./p2p

test-tls-auth:
	@go test -race -count=3 -run '^TestRun_tls' ./...

test-mtls:
	@go test -race -count=3 -run '^TestTCPTransport_mutualTLS' ./p2p

test-mtls-cli:
	@go test -race -count=3 -run '^TestRun_tlsMutualTLS' ./...

test-tls-rotation:
	@go test -race -count=3 -run '^TestRun_tlsRestartRotation' ./...

test-tls-credential-health:
	@go test -race -count=3 -run '^Test(Run_tlsCredentialHealth|TLSCredentialHealth)' ./...

test-prometheus-format:
	@go test -race -count=3 -run '^Test(WritePrometheusMetrics|Prometheus)' ./...

test-health-server:
	@go test -race -count=3 -run '^TestRun_healthServer' ./...

test-cli-shutdown:
	@go test -race -count=3 -run "^Test(Run_(shutdownGraceRejectsNonPositive|contextCancellationUsesBoundedShutdown|exitAfterPutCancellationDrainsTransport)|ShutdownApplicationDeadlineExpiresAndJoins|PeerReconnector_doesNotScheduleAfterCancellation|RepairContinuationSchedulerStopsOnShutdown)$$" ./...

test-lifecycle-compaction:
	@go test -race -count=1 -run '^TestRunLifecycleSnapshotRepairsStalePeerAfterCompaction$$' ./...

test-lifecycle-membership:
	@go test -race -count=3 -run '^TestRunLifecycleCompactionBlocksUntilMemberReconnects$$' ./...

test-lifecycle-compose:
	@./scripts/demo-lifecycle-compose.sh

test-fuzz:
	@go test -run '^$$' -fuzz=FuzzDecode -fuzztime=3s ./replication
	@go test -run '^$$' -fuzz=FuzzEncodeDecodeBlobPut -fuzztime=3s ./replication
	@go test -run '^$$' -fuzz=FuzzReadFrame -fuzztime=3s ./p2p
	@go test -run '^$$' -fuzz=FuzzWriteReadFrame -fuzztime=3s ./p2p

test-budgets:
	@go test -race -count=1 -run '^TestRepairContinuationSchedulerSaturatesAtConfiguredKeyBudget$$' ./...

bench-inventory:
	@go test -run '^$$' -bench '^Benchmark(MemoryStoreList(Keys|KeyPages)(4096|65536)|FileStore(ListKeys|ListKeyPages|BuildIndex)(4096|65536))$$' -benchmem -benchtime=100ms ./storage

bench-inventory-wire:
	@go test -run '^TestResearchInventoryWireFrameBudget$$' -bench '^BenchmarkResearch(BudgetedInventoryExchange|InventoryExchange)$$' -benchmem -benchtime=100ms .

vet:
	@go vet ./...

cover:
	@go test -count=1 -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out

lint:
	@golangci-lint run ./...

demo-replication:
	@./scripts/demo-replication.sh

demo-compose:
	@./scripts/demo-compose.sh

demo-auth:
	@./scripts/demo-auth.sh

demo-repair:
	@./scripts/demo-repair.sh

demo-failure:
	@./scripts/demo-failure.sh

demo-continuation:
	@./scripts/demo-continuation.sh

demo-inventory-budget:
	@./scripts/demo-inventory-budget.sh

demo-status:
	@./scripts/demo-status.sh

ci: vet test-race lint

help:
	@echo "Targets: build run test test-race test-fairness test-inventory-fairness test-eviction-repair test-inventory-consistency test-inventory-status test-peer-admission test-peer-drain test-tls-auth test-mtls test-cli-shutdown test-lifecycle-compaction test-lifecycle-membership test-lifecycle-compose test-tls-rotation test-tls-credential-health test-prometheus-format test-health-server test-fuzz test-budgets bench-inventory bench-inventory-wire vet cover lint demo-replication demo-compose demo-auth demo-repair demo-failure demo-continuation demo-inventory-budget demo-status ci"
