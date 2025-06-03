#!/usr/bin/env python3
"""
Script to check if the last 5 Ethereum blocks contain empty transactions.

Usage:
    uv run check_last_5_blocks_not_empty.py --url http://localhost:8545
    uv run check_last_5_blocks_not_empty.py --url http://localhost:8545 --transactions 10
"""

import argparse
import requests
import sys

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

def main():
    parser = argparse.ArgumentParser(
        description="Check if the last 5 Ethereum blocks have empty transactions."
    )
    parser.add_argument("--url", required=True, help="JRPC Node URL")
    parser.add_argument("--log", required=False, help="Print blocks if True")
    parser.add_argument("--transactions", type=int, required=False, help="Max number of transactions allowed per block")
    args = parser.parse_args()

    try:
        latest_block = get_latest_block_number(args.url)
    except Exception as e:
        print("Error retrieving latest block:", e)
        sys.exit(1)

    check_passed = True
    for block_number in range(latest_block, latest_block - 5, -1):
        block = get_block(args.url, block_number)
        if args.log:
            print(block)
        if block is None:
            print(f"Block {block_number} not found")
            sys.exit(1)
        transactions = block.get("transactions", [])

        if args.transactions is not None:
            # Check if number of transactions is less than the provided limit
            if len(transactions) >= args.transactions:
                check_passed = False
                break
        else:
            # Original behavior: check if any block has transactions
            if transactions:
                check_passed = False
                break

    print("true" if check_passed else "false")

if __name__ == "__main__":
    main()
