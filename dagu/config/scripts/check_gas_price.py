#!/usr/bin/env python3
"""
Script to check if the current Ethereum gas price is below a minimum threshold.

Usage:
    uv run check_gas_price.py --url http://localhost:8545 --min_gas_price_wei 1000000000
    uv run check_gas_price.py --url http://localhost:8545 --min_gas_price_wei 1000000000 --wait --timeout 1800
"""

import argparse
import requests
import sys
import time

def get_gas_price(url):
    """
    Retrieves the current gas price from the given JSON-RPC endpoint.
    """
    data = {
        "jsonrpc": "2.0",
        "method": "eth_gasPrice",
        "params": [],
        "id": 0
    }
    response = requests.post(url, json=data)
    response.raise_for_status()
    result = response.json()['result']
    return int(result, 16)

def is_gas_price_below_threshold(url, min_gas_price_wei, log):
    current_gas_price = get_gas_price(url)
    if log:
        print(f"Current gas price: {current_gas_price} wei")
    return current_gas_price < min_gas_price_wei

def main():
    parser = argparse.ArgumentParser(
        description="Check if the current Ethereum gas price is below a minimum threshold."
    )
    parser.add_argument("--url", required=True, help="JRPC Node URL")
    parser.add_argument("--min_gas_price_wei", required=True, type=int,
                        help="Minimum gas price threshold in wei")
    parser.add_argument("--log", action="store_true", help="Print gas price if True")
    parser.add_argument("--wait", action="store_true", help="Wait until the condition is met")
    parser.add_argument("--timeout", type=int, default=1800, help="Max wait time in seconds (default: 1800)")
    args = parser.parse_args()

    start_time = time.time()

    if args.wait:
        while True:
            try:
                if is_gas_price_below_threshold(args.url, args.min_gas_price_wei, args.log):
                    print("true")
                    sys.exit(0)
            except Exception as e:
                print(f"Error retrieving gas price: {e}")
                sys.exit(1)
            if time.time() - start_time > args.timeout:
                print("false")
                sys.exit(1)
            time.sleep(10)
    else:
        try:
            result = is_gas_price_below_threshold(args.url, args.min_gas_price_wei, args.log)
            print("true" if result else "false")
        except Exception as e:
            print(f"Error retrieving gas price: {e}")
            sys.exit(1)

if __name__ == "__main__":
    main()
