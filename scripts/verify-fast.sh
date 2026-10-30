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
erpc_procs=${VERIFY_ERPC_PROCS:-$((cpus / 2))}
chaos='^TestChainFeed_ConvergesUnderFaults$'
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
	"$dir/erpc.test" -test.list '.*' | grep -E '^Test' | grep -vE "$chaos" >"$dir/erpc/tests.txt"
	: >"$dir/erpc/results.txt"
	xargs -P "$erpc_procs" -I{} bash -c '
		s=$(date +%s)
		"$0" -test.run "^{}\$" -test.count 1 -test.timeout 8m -test.v >"$1/{}.log" 2>&1
		printf "%s %d %ds\n" {} $? $(($(date +%s) - s)) >>"$1/results.txt"
	' "$dir/erpc.test" "$dir/erpc" <"$dir/erpc/tests.txt"
	echo "$(wc -l <"$dir/erpc/results.txt") of $(wc -l <"$dir/erpc/tests.txt") tests ran"
	local fails
	fails=$(awk '$2 != 0 {print $1}' "$dir/erpc/results.txt")
	[ -z "$fails" ] && return 0
	for t in $fails; do echo "FAIL	$t ($dir/erpc/$t.log)"; done
	return 1
}

start=$(date +%s)
pkgs=$(go list ./... | grep -vE '/erpc/erpc$|/test$|/test/realworld$')
if part build go build ./... && part vet go vet ./...; then
	part chaos go test -race -count=1 -timeout 9m -run "$chaos" ./erpc/ &
	part race-pkgs go test -race -count=1 -timeout 9m $pkgs &
	part erpc-shards erpc_tests &
	wait
fi
row go-total 0 $(($(date +%s) - start)) ""
rw=$(date +%s)
part realworld env REALWORLD_TIER=fast go test ./test/realworld/ -run TestRealWorld -count=1 -v -timeout 15m
skipped=$(awk '$3 == "skip" {print $2}' "$dir/realworld.log" | sort -u | tr '\n' ' ')
row realworld-long 0 0 "not run, make test-realworld-long runs: ${skipped:-none}"
row total 0 $(($(date +%s) - start)) "realworld $(($(date +%s) - rw))s"

echo
printf '%-16s %-5s %6s  %s\n' part result wall "failed / note"
cat "$table"
echo "verify: logs in $dir"
! grep -q ' FAIL ' "$table"
