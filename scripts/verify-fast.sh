#!/usr/bin/env bash
# Full verification in minutes: build, vet, -race tests of every package
# (./erpc/ one test per process, gock being process-global; the chaos test
# once), then the real-world suite's fast tier. Prints a pass/fail table
# with wall times; logs go to VERIFY_DIR or a new directory it names.
set -u
cd "$(dirname "$0")/.."
dir=${VERIFY_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/erpc-verify-XXXXXX")}
mkdir -p "$dir/erpc"
dir=$(cd "$dir" && pwd)
echo "verify: logs in $dir"
cpus=$(getconf _NPROCESSORS_ONLN)
erpc_procs=${VERIFY_ERPC_PROCS:-$cpus}
chaos='^TestChainFeed_ConvergesUnderFaults$'
# Tests that start containers (testcontainers): no Docker on this laptop.
docker='^(TestDynamoDB|TestPostgre|TestEvmJsonRpcCache_(DynamoDB|Redis)$)'
table=$dir/table.txt
: >"$table"

# part NAME CMD...: runs CMD with its output in NAME.log and adds its row.
part() {
	local name=$1 s rc
	shift
	s=$(date +%s)
	"$@" >"$dir/$name.log" 2>&1
	rc=$?
	row "$name" "$rc" $(($(date +%s) - s)) "$(failed "$dir/$name.log")"
	return $rc
}

row() {
	local result=pass
	[ "$2" -ne 0 ] && result=FAIL
	printf '%-16s %-5s %5ss  %s\n' "$1" "$result" "$3" "$4" >>"$table"
}

# failed: the deepest tests a go test log reports failed, and failed
# packages, at most 12.
failed() {
	grep -hoE '^ *--- FAIL: [^ ]+|^FAIL	[^ ]+' "$1" | sed -E 's/^ *--- FAIL: //; s/^FAIL	//' | sort -u |
		awk '{n[NR] = $0} END {for (i = 1; i <= NR; i++) if (index(n[i+1], n[i] "/") != 1) out[++k] = n[i]
			for (i = 1; i <= k && i <= 12; i++) printf "%s ", out[i]; if (k > 12) printf "(+%d more)", k - 12}'
}

# erpc_tests runs every ./erpc/ test but the chaos test, each in its own
# process of one -race test binary, erpc_procs at a time.
erpc_tests() {
	go test -race -c -o "$dir/erpc.test" ./erpc || return 1
	cd erpc || return 1
	"$dir/erpc.test" -test.list '.*' | grep -E '^Test' | grep -vE "$chaos|$docker" >"$dir/erpc/tests.txt"
	: >"$dir/erpc/results.txt"
	xargs -P "$erpc_procs" -n 1 bash -c '
		bin=$1 out=$2 t=$3 s=$(date +%s)
		"$bin" -test.run "^$t\$" -test.count 1 -test.timeout 8m -test.v >"$out/$t.log" 2>&1
		printf "%s %d %ds\n" "$t" $? $(($(date +%s) - s)) >>"$out/results.txt"
	' _ "$dir/erpc.test" "$dir/erpc" <"$dir/erpc/tests.txt"
	local ran listed
	ran=$(wc -l <"$dir/erpc/results.txt") listed=$(wc -l <"$dir/erpc/tests.txt")
	echo "$ran of $listed tests ran"
	[ "$ran" -eq "$listed" ] || { echo "FAIL	$((listed - ran)) tests never ran"; return 1; }
	local fails
	fails=$(awk '$2 != 0 {print $1}' "$dir/erpc/results.txt")
	[ -z "$fails" ] && return 0
	for t in $fails; do echo "FAIL	$t ($dir/erpc/$t.log)"; done
	return 1
}

# docker_tests counts the test functions the docker pattern skips.
docker_tests() {
	grep -rhoE --include='*_test.go' '^func Test[A-Za-z0-9_]+' . | sed 's/^func //' | grep -cE "$docker"
}

start=$(date +%s)
pkgs=$(go list ./... | grep -vE '/erpc/erpc$|/test$|/test/realworld$')
if part build go build ./... && part vet go vet ./...; then
	go test -c -o "$dir/realworld.test" ./test/realworld/ >"$dir/realworld-build.log" 2>&1 &
	part chaos go test -race -count=1 -timeout 9m -run "$chaos" ./erpc/ &
	part race-pkgs go test -race -count=1 -timeout 9m -skip "$docker" $pkgs &
	part erpc-shards erpc_tests &
	wait
fi
row go-total 0 $(($(date +%s) - start)) ""
rw=$(date +%s)
part realworld bash -c "cd test/realworld && REALWORLD_TIER=fast '$dir/realworld.test' -test.run TestRealWorld -test.count 1 -test.v -test.timeout 9m"
for tier in full long; do
	n=$(awk -v t="$tier tier" '$3 == "skip" && index($0, t) {print $2}' "$dir/realworld.log" | sort -u | wc -l | tr -d ' ')
	row "realworld-$tier" 0 0 "not run: $n scenarios (make test-realworld$([ $tier = long ] && echo -long))"
done
row docker 0 0 "skipped: needs Docker ($(docker_tests))"
row total 0 $(($(date +%s) - start)) "realworld $(($(date +%s) - rw))s"
echo
printf '%-16s %-5s %6s  %s\n' part result wall "failed / note"
cat "$table"
echo "verify: logs in $dir"
! grep -q ' FAIL ' "$table"
