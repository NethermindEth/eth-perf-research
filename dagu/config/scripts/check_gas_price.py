#!/usr/bin/env python3
"""
Script to check if the current Ethereum gas price is below a minimum threshold.

Usage:
    uv run check_gas_price.py --url http://localhost:8545 --min_gas_price_wei 1000000000
"""

import argparse
import requests
import sys

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

def main():
    parser = argparse.ArgumentParser(
        description="Check if the current Ethereum gas price is below a minimum threshold."
    )
    parser.add_argument("--url", required=True, help="JRPC Node URL")
    parser.add_argument("--min_gas_price_wei", required=True, type=int, 
                        help="Minimum gas price threshold in wei")
    parser.add_argument("--log", action="store_true", help="Print gas price if True")
    args = parser.parse_args()

    try:
        current_gas_price = get_gas_price(args.url)
        if args.log:
            print(f"Current gas price: {current_gas_price} wei")
        
        is_below_threshold = current_gas_price < args.min_gas_price_wei
        print("true" if is_below_threshold else "false")
        
    except Exception as e:
        print(f"Error retrieving gas price: {e}")
        sys.exit(1)

if __name__ == "__main__":
    main()