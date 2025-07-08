#!/usr/bin/env python3
"""
Script to add, update, or read variables in a JSON config file.

Usage:
    python make_config.py --name=my_variable --value=10
    python make_config.py --name=my_variable --value=10 --file_name=custom.json
    python make_config.py --name=my_variable --read
    python make_config.py --name=my_variable --read --file_name=custom.json
    python make_config.py --name=my_variable --read --value=10

- Adds or updates a variable in the specified JSON file (default: config.json).
- With --read, prints the value of the variable if it exists, otherwise prints the value of --value if provided, or "null".
"""

import argparse
import json
import os

def main():
    parser = argparse.ArgumentParser(description="Add or update a variable in config.json, or read its value")
    parser.add_argument("--name", required=True, help="Variable name")
    parser.add_argument("--value", required=False, help="Variable value")
    parser.add_argument("--read", action="store_true", help="Read variable value from config.json")
    parser.add_argument("--file_name", required=False, default="config.json", help="JSON file to use (default: config.json)")
    args = parser.parse_args()

    config_path = os.path.join(os.path.dirname(__file__), args.file_name)

    # Load existing config if it exists, otherwise start with empty dict
    if os.path.exists(config_path):
        with open(config_path, "r") as f:
            try:
                config = json.load(f)
            except json.JSONDecodeError:
                config = {}
    else:
        config = {}

    if args.read:
        # Read mode: print value if exists, else fallback to --value or "null"
        value = config.get(args.name)
        if value is not None:
            print(value)
        elif args.value is not None:
            print(args.value)
        else:
            print("null")
        return

    # Update or add the variable
    if args.value is None:
        print("Error: --value is required when not using --read")
        return
    config[args.name] = args.value

    # Write back to config.json
    with open(config_path, "w") as f:
        json.dump(config, f, indent=4)
        f.write("\n")

if __name__ == "__main__":
    main()
