---
name: Bug report
about: Create a report to help us improve
title: 'bug: summary of your issue'
labels: bug
assignees: ''

---

**Describe the bug**
A clear and concise description of what the bug is.

**To Reproduce**
Steps to reproduce the behavior:
1. Use this erpc.yaml config:
```yaml
logLevel: debug
projects:
# ...
```
2. Make this request:
```bash
curl --location 'http://localhost:4000/main/evm/42161' \
--header 'Content-Type: application/json' \
--data '{
    "method": "eth_getBlockByNumber",
    "params": [
        "0x1203319",
        false
    ],
    "id": 9199,
    "jsonrpc": "2.0"
}'
```

**Expected behavior**
A clear and concise description of what you expected to happen.

**Screenshots**
If applicable, add screenshots to help explain your problem.

**Relevant Logs:**
```jsonl
// ...
```

**Additional context**
Add any other context about the problem here.
