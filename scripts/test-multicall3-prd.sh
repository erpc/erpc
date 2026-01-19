#!/bin/bash
# =============================================================================
# Multicall3 Aggregation Production Test Script
# =============================================================================
# This script tests if multicall3 batching is working correctly in production.
#
# Prerequisites:
# - jq installed
# - curl installed
# - Access to eRPC endpoint
# - Access to Prometheus/metrics endpoint (optional but recommended)
#
# Usage:
#   ./scripts/test-multicall3-prd.sh <erpc-endpoint> [metrics-endpoint]
#
# Examples:
#   ./scripts/test-multicall3-prd.sh https://erpc.example.com/main/evm/1
#   ./scripts/test-multicall3-prd.sh https://erpc.example.com/main/evm/1 https://erpc.example.com/metrics
# =============================================================================

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
ERPC_ENDPOINT="${1:-}"
METRICS_ENDPOINT="${2:-}"

# Multicall3 contract address (same on most EVM chains)
MULTICALL3_ADDRESS="0xcA11bde05977b3631167028862bE2a173976CA11"

# Test contract addresses (well-known contracts on mainnet)
# WETH on Ethereum mainnet
WETH_ADDRESS="0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"
# USDC on Ethereum mainnet
USDC_ADDRESS="0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"
# DAI on Ethereum mainnet
DAI_ADDRESS="0x6B175474E89094C44Da98b954EesdeAfe4068538"

# Function signatures (for eth_call)
# balanceOf(address) = 0x70a08231
# decimals() = 0x313ce567
# symbol() = 0x95d89b41
# totalSupply() = 0x18160ddd

usage() {
    echo "Usage: $0 <erpc-endpoint> [metrics-endpoint]"
    echo ""
    echo "Examples:"
    echo "  $0 https://erpc.example.com/main/evm/1"
    echo "  $0 https://erpc.example.com/main/evm/1 https://erpc.example.com/metrics"
    exit 1
}

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[PASS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[FAIL]${NC} $1"
}

log_section() {
    echo ""
    echo -e "${BLUE}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${BLUE}  $1${NC}"
    echo -e "${BLUE}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
}

# Validate arguments
if [[ -z "$ERPC_ENDPOINT" ]]; then
    usage
fi

# =============================================================================
# Test 1: Basic connectivity
# =============================================================================
log_section "Test 1: Basic Connectivity"

log_info "Testing endpoint connectivity..."
CHAIN_ID_RESPONSE=$(curl -s -X POST "$ERPC_ENDPOINT" \
    -H "Content-Type: application/json" \
    -d '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}')

if echo "$CHAIN_ID_RESPONSE" | jq -e '.result' > /dev/null 2>&1; then
    CHAIN_ID=$(echo "$CHAIN_ID_RESPONSE" | jq -r '.result')
    log_success "Endpoint reachable, chainId: $CHAIN_ID"
else
    log_error "Failed to connect to endpoint"
    echo "Response: $CHAIN_ID_RESPONSE"
    exit 1
fi

# =============================================================================
# Test 2: Capture baseline metrics (if metrics endpoint provided)
# =============================================================================
BASELINE_AGGREGATION_COUNT=""
BASELINE_FALLBACK_COUNT=""

if [[ -n "$METRICS_ENDPOINT" ]]; then
    log_section "Test 2: Capture Baseline Metrics"

    log_info "Fetching baseline multicall3 metrics..."
    METRICS=$(curl -s "$METRICS_ENDPOINT" 2>/dev/null || echo "")

    if [[ -n "$METRICS" ]]; then
        BASELINE_AGGREGATION_COUNT=$(echo "$METRICS" | grep 'erpc_multicall3_aggregation_total' | grep -v '^#' | awk '{sum += $2} END {print sum}' || echo "0")
        BASELINE_FALLBACK_COUNT=$(echo "$METRICS" | grep 'erpc_multicall3_fallback_total' | grep -v '^#' | awk '{sum += $2} END {print sum}' || echo "0")

        log_success "Baseline aggregation count: ${BASELINE_AGGREGATION_COUNT:-0}"
        log_success "Baseline fallback count: ${BASELINE_FALLBACK_COUNT:-0}"
    else
        log_warning "Could not fetch metrics, skipping metric-based verification"
    fi
else
    log_section "Test 2: Metrics Check (Skipped)"
    log_warning "No metrics endpoint provided, skipping metric-based verification"
fi

# =============================================================================
# Test 3: Single eth_call (should NOT be batched - baseline)
# =============================================================================
log_section "Test 3: Single eth_call (Baseline)"

log_info "Sending single eth_call request..."
# Call decimals() on WETH
SINGLE_CALL_RESPONSE=$(curl -s -X POST "$ERPC_ENDPOINT" \
    -H "Content-Type: application/json" \
    -d '{
        "jsonrpc":"2.0",
        "id":1,
        "method":"eth_call",
        "params":[{
            "to":"'"$WETH_ADDRESS"'",
            "data":"0x313ce567"
        },"latest"]
    }')

if echo "$SINGLE_CALL_RESPONSE" | jq -e '.result' > /dev/null 2>&1; then
    DECIMALS=$(echo "$SINGLE_CALL_RESPONSE" | jq -r '.result')
    log_success "Single eth_call succeeded, decimals: $DECIMALS"
else
    log_error "Single eth_call failed"
    echo "Response: $SINGLE_CALL_RESPONSE"
fi

# =============================================================================
# Test 4: JSON-RPC batch with multiple eth_calls (should be batched via multicall3)
# =============================================================================
log_section "Test 4: JSON-RPC Batch (Multiple eth_calls)"

log_info "Sending JSON-RPC batch with 3 eth_call requests..."
log_info "These should be aggregated into a single multicall3 call"

BATCH_RESPONSE=$(curl -s -X POST "$ERPC_ENDPOINT" \
    -H "Content-Type: application/json" \
    -d '[
        {"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"'"$WETH_ADDRESS"'","data":"0x313ce567"},"latest"]},
        {"jsonrpc":"2.0","id":2,"method":"eth_call","params":[{"to":"'"$WETH_ADDRESS"'","data":"0x95d89b41"},"latest"]},
        {"jsonrpc":"2.0","id":3,"method":"eth_call","params":[{"to":"'"$WETH_ADDRESS"'","data":"0x18160ddd"},"latest"]}
    ]')

# Check if all 3 responses are valid
VALID_COUNT=$(echo "$BATCH_RESPONSE" | jq '[.[] | select(.result != null)] | length')

if [[ "$VALID_COUNT" == "3" ]]; then
    log_success "JSON-RPC batch returned 3 valid responses"

    # Extract results
    DECIMALS=$(echo "$BATCH_RESPONSE" | jq -r '.[0].result')
    SYMBOL=$(echo "$BATCH_RESPONSE" | jq -r '.[1].result')
    TOTAL_SUPPLY=$(echo "$BATCH_RESPONSE" | jq -r '.[2].result')

    log_info "  decimals(): $DECIMALS"
    log_info "  symbol(): $SYMBOL"
    log_info "  totalSupply(): $TOTAL_SUPPLY"
else
    log_error "JSON-RPC batch did not return 3 valid responses"
    echo "Response: $BATCH_RESPONSE"
fi

# =============================================================================
# Test 5: Concurrent requests (should be batched together)
# =============================================================================
log_section "Test 5: Concurrent Requests (Batching Test)"

log_info "Sending 5 concurrent eth_call requests..."
log_info "These should be batched together within the window"

# Create temp files for responses
TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

# Send 5 concurrent requests
for i in {1..5}; do
    curl -s -X POST "$ERPC_ENDPOINT" \
        -H "Content-Type: application/json" \
        -d '{"jsonrpc":"2.0","id":'$i',"method":"eth_call","params":[{"to":"'"$WETH_ADDRESS"'","data":"0x313ce567"},"latest"]}' \
        > "$TEMP_DIR/response_$i.json" &
done

# Wait for all requests to complete
wait

# Check responses
SUCCESS_COUNT=0
for i in {1..5}; do
    if jq -e '.result' "$TEMP_DIR/response_$i.json" > /dev/null 2>&1; then
        ((SUCCESS_COUNT++))
    fi
done

if [[ "$SUCCESS_COUNT" == "5" ]]; then
    log_success "All 5 concurrent requests succeeded"
else
    log_error "Only $SUCCESS_COUNT/5 concurrent requests succeeded"
fi

# =============================================================================
# Test 6: Mixed batch (eth_call + other methods)
# =============================================================================
log_section "Test 6: Mixed Batch (eth_call + other methods)"

log_info "Sending mixed batch with eth_call and eth_blockNumber..."

MIXED_BATCH_RESPONSE=$(curl -s -X POST "$ERPC_ENDPOINT" \
    -H "Content-Type: application/json" \
    -d '[
        {"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"'"$WETH_ADDRESS"'","data":"0x313ce567"},"latest"]},
        {"jsonrpc":"2.0","id":2,"method":"eth_blockNumber","params":[]},
        {"jsonrpc":"2.0","id":3,"method":"eth_call","params":[{"to":"'"$WETH_ADDRESS"'","data":"0x18160ddd"},"latest"]}
    ]')

VALID_COUNT=$(echo "$MIXED_BATCH_RESPONSE" | jq '[.[] | select(.result != null)] | length')

if [[ "$VALID_COUNT" == "3" ]]; then
    log_success "Mixed batch returned 3 valid responses"
else
    log_error "Mixed batch did not return 3 valid responses"
    echo "Response: $MIXED_BATCH_RESPONSE"
fi

# =============================================================================
# Test 7: eth_call that reverts (error handling)
# =============================================================================
log_section "Test 7: eth_call That Reverts (Error Handling)"

log_info "Sending eth_call that should revert..."
# Call a non-existent function on a contract
REVERT_RESPONSE=$(curl -s -X POST "$ERPC_ENDPOINT" \
    -H "Content-Type: application/json" \
    -d '{
        "jsonrpc":"2.0",
        "id":1,
        "method":"eth_call",
        "params":[{
            "to":"'"$WETH_ADDRESS"'",
            "data":"0xdeadbeef"
        },"latest"]
    }')

# Check if we got an error or empty result (both are acceptable for invalid call)
if echo "$REVERT_RESPONSE" | jq -e '.error' > /dev/null 2>&1; then
    ERROR_MSG=$(echo "$REVERT_RESPONSE" | jq -r '.error.message // .error')
    log_success "Revert handled correctly with error: $ERROR_MSG"
elif echo "$REVERT_RESPONSE" | jq -e '.result' > /dev/null 2>&1; then
    RESULT=$(echo "$REVERT_RESPONSE" | jq -r '.result')
    if [[ "$RESULT" == "0x" ]] || [[ -z "$RESULT" ]]; then
        log_success "Revert returned empty result (expected)"
    else
        log_warning "Got unexpected result for invalid call: $RESULT"
    fi
else
    log_warning "Unexpected response format for revert test"
    echo "Response: $REVERT_RESPONSE"
fi

# =============================================================================
# Test 8: Verify metrics increased (if metrics endpoint provided)
# =============================================================================
if [[ -n "$METRICS_ENDPOINT" ]] && [[ -n "$BASELINE_AGGREGATION_COUNT" ]]; then
    log_section "Test 8: Verify Metrics Increased"

    # Wait a moment for metrics to be updated
    sleep 2

    log_info "Fetching updated multicall3 metrics..."
    METRICS=$(curl -s "$METRICS_ENDPOINT" 2>/dev/null || echo "")

    if [[ -n "$METRICS" ]]; then
        NEW_AGGREGATION_COUNT=$(echo "$METRICS" | grep 'erpc_multicall3_aggregation_total' | grep -v '^#' | awk '{sum += $2} END {print sum}' || echo "0")
        NEW_FALLBACK_COUNT=$(echo "$METRICS" | grep 'erpc_multicall3_fallback_total' | grep -v '^#' | awk '{sum += $2} END {print sum}' || echo "0")

        AGGREGATION_DIFF=$((${NEW_AGGREGATION_COUNT:-0} - ${BASELINE_AGGREGATION_COUNT:-0}))
        FALLBACK_DIFF=$((${NEW_FALLBACK_COUNT:-0} - ${BASELINE_FALLBACK_COUNT:-0}))

        log_info "Aggregation count: ${BASELINE_AGGREGATION_COUNT:-0} -> ${NEW_AGGREGATION_COUNT:-0} (+$AGGREGATION_DIFF)"
        log_info "Fallback count: ${BASELINE_FALLBACK_COUNT:-0} -> ${NEW_FALLBACK_COUNT:-0} (+$FALLBACK_DIFF)"

        if [[ "$AGGREGATION_DIFF" -gt 0 ]]; then
            log_success "Multicall3 aggregation is working! ($AGGREGATION_DIFF new aggregations)"
        else
            log_warning "No new aggregations detected - multicall3 might not be enabled or requests fell back"
        fi

        if [[ "$FALLBACK_DIFF" -gt 0 ]]; then
            log_warning "$FALLBACK_DIFF requests fell back to individual calls"
        fi

        # Show other relevant metrics
        log_info ""
        log_info "Additional metrics:"
        echo "$METRICS" | grep 'erpc_multicall3_' | grep -v '^#' | head -20 || true
    else
        log_warning "Could not fetch updated metrics"
    fi
fi

# =============================================================================
# Summary
# =============================================================================
log_section "Test Summary"

echo ""
echo "Tests completed. Review the output above to verify:"
echo ""
echo "  1. ✓ Basic connectivity works"
echo "  2. ✓ Single eth_call works (baseline)"
echo "  3. ✓ JSON-RPC batch with multiple eth_calls works"
echo "  4. ✓ Concurrent requests are handled"
echo "  5. ✓ Mixed batches work correctly"
echo "  6. ✓ Reverts are handled gracefully"
echo ""
echo "To confirm multicall3 batching is actually happening:"
echo ""
echo "  • Check erpc_multicall3_aggregation_total metric increased"
echo "  • Check erpc_multicall3_batch_size histogram for batch sizes > 1"
echo "  • Check logs for 'multicall3' mentions"
echo ""

if [[ -n "$METRICS_ENDPOINT" ]]; then
    echo "Useful PromQL queries:"
    echo ""
    echo "  # Aggregation rate"
    echo "  rate(erpc_multicall3_aggregation_total[5m])"
    echo ""
    echo "  # Average batch size"
    echo "  histogram_quantile(0.5, rate(erpc_multicall3_batch_size_bucket[5m]))"
    echo ""
    echo "  # Fallback rate (should be low)"
    echo "  rate(erpc_multicall3_fallback_total[5m])"
    echo ""
fi
