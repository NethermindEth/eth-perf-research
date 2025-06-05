# Dagu Perfnet Orchestrator

Dagu is a powerful, self-contained alternative to Cron, featuring a clean web UI and a declarative YAML-based workflow definition. It simplifies complex job dependencies and scheduling with minimal overhead, making it ideal for orchestrating benchmarking, research, and infrastructure tasks.

In The Perfnet, we are using Dagu to orchestrate test running (e.g. start and stop `spamoor` after specific conditions).

---

## 🚀 Features

- **Declarative DAGs:** Define workflows as YAML files with clear dependencies.
- **Web UI:** Monitor, trigger, and debug jobs visually.
- **Flexible Execution:** Run shell commands, Docker containers, HTTP requests, and more.
- **Repeat & Conditional Logic:** Advanced step repetition and preconditions.
- **Notifications:** Email alerts on job success or failure.

We are using a modified `docker-compose.yml` with following features:
- Installed `uv` and python inside dagu container
- All dags are stored inside `/root/.config/dagu/dags`
- There are a directory with python scripts inside `/root/.config/dagu/scripts`
- Mount `/root/.local` to save all the runs and history
- Mount `/var/run/docker.sock` to be able to interact with other docker containers on the same host

---

## 🐳 Quick Start with Docker Compose

You can run Dagu easily using Docker. The following command will start Dagu and expose the web UI on port 8080:

```sh
cd dagu
docker compose up -d
```

- Access the web UI at [http://localhost:8092](http://localhost:8092)
- Place your DAG YAML files in `/root/.config/dagu/dags` or `config/dags` from the host.

---

## 📝 Writing DAGs

DAGs are defined in YAML format. Each DAG consists of parameters and steps, with support for dependencies, output passing, conditional execution, and more.

### Hello World Example

```yaml
params:
  - NAME: "Dagu"
steps:
  - name: Hello world
    command: echo Hello $NAME
  - name: Done
    command: echo Done!
    depends:
      - Hello world
```

### Conditional Steps

```yaml
params: foo
steps:
  - name: step1
    command: echo start
  - name: foo
    command: echo foo
    depends:
      - step1
    preconditions:
      - condition: "$1"
        expected: foo
  - name: bar
    command: echo bar
    depends:
      - step1
    preconditions:
      - condition: "$1"
        expected: bar
```

### File Output

```yaml
steps:
  - name: write hello to '/tmp/hello.txt'
    command: echo hello
    stdout: /tmp/hello.txt
```

### Passing Output to Next Step

```yaml
steps:
  - name: pass 'hello'
    command: echo hello
    output: OUT1
  - name: output 'hello world'
    command: bash
    script: |
      echo $OUT1 world
    depends:
      - pass 'hello'
```

---

## 🐍 Using python scripts

The Perfnet orchestrator integrates several Python scripts for advanced workflow control and on-chain condition checks. All scripts are located in `config/scripts` which is mapped to `/root/.config/dagu/scripts/` inside dagu container. All the dependencies are listed in `requirements.txt` (install with `uv pip install -r requirements.txt` inside the container).

### Available Scripts

- **check_gas_price.py**
  Checks if the current Ethereum gas price is below a minimum threshold.
  Example usage:
  ```
  uv run check_gas_price.py --url http://localhost:8545 --min_gas_price_wei 1000000000
  uv run check_gas_price.py --url http://localhost:8545 --min_gas_price_wei 1000000000 --wait --timeout 1800
  ```
  - Supports waiting until the condition is met (`--wait`) and custom timeouts.

- **check_last_5_blocks_not_empty.py**
  Checks if the last 5 Ethereum blocks contain empty transactions or are below a transaction threshold.
  Example usage:
  ```
  uv run check_last_5_blocks_not_empty.py --url http://localhost:8545
  uv run check_last_5_blocks_not_empty.py --url http://localhost:8545 --transactions 10
  uv run check_last_5_blocks_not_empty.py --url http://localhost:8545 --wait --timeout 1800
  ```
  - Can be used to ensure blocks are empty or have fewer than a set number of transactions.

- **make_config.py**
  Adds, updates, or reads variables in a JSON config file (default: `config.json`).
  Example usage:
  ```
  python make_config.py --name=my_variable --value=10
  python make_config.py --name=my_variable --read
  python make_config.py --name=my_variable --read --file_name=custom.json
  ```
  - Use `--read` to fetch a value, or omit to set/update a value.

> All scripts require Python 3 and the `requests` library (see `requirements.txt`).

### Using scripts inside DAGs

All the scripts returns some string output (usually true or false) and exits with status code 1 in case of failure.
To use it inside dags, create a dag to execute the script, e.g.:

```yaml
params:
  - VARIABLE_NAME: "IS_RUN_FINISHED"
  - VARIABLE_DEFAULT_VALUE: "true"
  - FILE_CONFIG_NAME: "config.json"
steps:
  - name: get variable VARIABLE_NAME
    command: uv run /root/.config/dagu/scripts/make_config.py --name=$VARIABLE_NAME --value=$VARIABLE_DEFAULT_VALUE --read --file_name=$FILE_CONFIG_NAME
    output: VARIABLE_VALUE
```

Then specify output variable `output: VARIABLE_OUTPUT` and reuse in the next step, something like `${VARIABLE_OUTPUT.outputs.VARIABLE_VALUE}`

```yaml
steps:
  - name: Get variable
    run: get_variable
    params: "VARIABLE_NAME=IS_RUN_FINISHED"
    output: VARIABLE_OUTPUT
  - name: Start spamers
    run: perfnet2_all_scenarios
    params: "SPAMMER_URL=$SPAMMER_URL TIMEOUT_SECONDS=$TIMEOUT_SECONDS RPC_URL=$RPC_URL MIN_GAS_LIMIT=$MIN_GAS_LIMIT"
    output: SPAMMER_RESULT
    continueOn:
      skipped: true
    preconditions:
      - condition: "${VARIABLE_OUTPUT.outputs.VARIABLE_VALUE}"
        expected: "true"
```

## 📚 References

- [Dagu Documentation](https://dagu.readthedocs.io/en/latest/)
- [Dagu GitHub](https://github.com/dagu-org/dagu)
