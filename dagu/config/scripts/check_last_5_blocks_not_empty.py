#!/usr/bin/env python3
"""
Script to check if the last 5 Ethereum blocks contain empty transactions.

Usage:
    uv run check_last_5_blocks_not_empty.py --url http://localhost:8545
    uv run check_last_5_blocks_not_empty.py --url http://localhost:8545 --transactions 10
    uv run check_last_5_blocks_not_empty.py --url http://localhost:8545 --wait --timeout 1800
"""

import argparse
import requests
import sys
import time

def get_latest_block_number(url):
    """
    Retrieves the latest block number from the given JSON-RPC endpoint.
    """
    data = {
         "jsonrpc": "2.0",
         "method": "eth_blockNumber",
         "params": [],
         "id": 1
    }
    response = requests.post(url, json=data)
    response.raise_for_status()
    result = response.json()['result']
    return int(result, 16)

def get_block(url, block_number):
    """
    Retrieves details for a block (with transactions) by block number.
    """
    hex_block = hex(block_number)
    data = {
        "jsonrpc": "2.0",
        "method": "eth_getBlockByNumber",
        "params": [hex_block, True],
        "id": 1
    }
    response = requests.post(url, json=data)
    response.raise_for_status()
    return response.json()['result']

def check_blocks(url, log, transactions_limit):
    """
    Checks the last 5 blocks for the transaction condition.
    Returns True if the condition is met, False otherwise.
    """
    try:
        latest_block = get_latest_block_number(url)
    except Exception as e:
        print("Error retrieving latest block:", e)
        sys.exit(1)

    for block_number in range(latest_block, latest_block - 5, -1):
        block = get_block(url, block_number)
        if log:
            print(block)
        if block is None:
            print(f"Block {block_number} not found")
            sys.exit(1)
        transactions = block.get("transactions", [])

        if transactions_limit is not None:
            if len(transactions) >= transactions_limit:
                return False
        else:
            if transactions:
                return False
    return True

def main():
    parser = argparse.ArgumentParser(
        description="Check if the last 5 Ethereum blocks have empty transactions."
    )
    parser.add_argument("--url", required=True, help="JRPC Node URL")
    parser.add_argument("--log", required=False, help="Print blocks if True")
    parser.add_argument("--transactions", type=int, required=False, help="Max number of transactions allowed per block")
    parser.add_argument("--wait", action="store_true", help="Wait until the condition is met")
    parser.add_argument("--fail", action="store_true", help="Exit with status 1 if sets to true")
    parser.add_argument("--timeout", type=int, default=1800, help="Max wait time in seconds (default: 1800)")
    args = parser.parse_args()

    log = bool(args.log)
    start_time = time.time()

    if args.wait:
        while True:
            if check_blocks(args.url, log, args.transactions):
                print("true")
                sys.exit(0)
            if time.time() - start_time > args.timeout:
                print("false")
                exit_code = 1 if args.fail else 0
                sys.exit(exit_code)
            time.sleep(10)
    else:
        result = check_blocks(args.url, log, args.transactions)
        print("true" if result else "false")

if __name__ == "__main__":
    main()
