"""Typer CLI: `orchestrator [run|--replay PATH]`.

All operator-level inputs (RPC URL, state dir, target YAML, environment fingerprint
fields) are required. Silent defaults are a footgun: they produce runs whose identity
(chain_identity_hash, manifest env fields) quietly diverges from what the operator
thinks they're launching. Fields that are *legitimately* optional — e.g. ``--max-batches``
(None = run until goal), ``--jwt-path`` (only needed on the engine port),
``--sensor-rpc-url`` (override of --rpc-url) — stay optional.
"""

from __future__ import annotations

import os
import platform
from pathlib import Path

import typer

from .lifecycle import run as lifecycle_run
from .manifest import EnvInfo
from .reference_f import default_reference_f_path, load_reference_f
from .replay import replay as replay_run
from .target import LiveTargetWatcher, load_target

app = typer.Typer(add_completion=False, help="Bloating feedback-loop orchestrator")


@app.callback(invoke_without_command=True)
def main(
    ctx: typer.Context,
    rpc_url: str = typer.Option(..., "--rpc-url", help="Nethermind JSON-RPC endpoint."),
    state_dir: Path = typer.Option(
        ...,
        "--state-dir",
        help="Directory for journal, manifest, payloads, and pending sidecar.",
    ),
    target_yaml: Path | None = typer.Option(
        None,
        "--target-yaml",
        help="Path to target.yaml (required when running; ignored under --replay).",
    ),
    genesis_sha256: str = typer.Option(
        ...,
        "--genesis-sha256",
        help="Hex sha256 of genesis.json; pins the chain identity in the manifest.",
    ),
    plugin_git_sha: str = typer.Option(
        ...,
        "--plugin-git-sha",
        help="Git SHA of the Nethermind statecomp plugin build.",
    ),
    nethermind_commit_sha: str = typer.Option(
        ...,
        "--nethermind-commit-sha",
        help="Git SHA of the Nethermind commit this run targeted.",
    ),
    dotnet_runtime_major: int = typer.Option(
        ...,
        "--dotnet-runtime-major",
        help="Major .NET runtime version Nethermind is running under.",
    ),
    replay: Path | None = typer.Option(
        None, "--replay", help="Replay a journal; no controller, no sensor."
    ),
    sensor_rpc_url: str | None = typer.Option(
        None,
        "--sensor-rpc-url",
        help="Override the URL used for statecomp_get (defaults to --rpc-url).",
    ),
    jwt_path: Path | None = typer.Option(
        None,
        "--jwt-path",
        envvar="JWT_PATH",
        help="Path to the 64-hex JWT for engine-protected RPC ports.",
    ),
    reference_f_path: Path | None = typer.Option(None, "--reference-f"),
    manifest_path: Path | None = typer.Option(None, "--manifest"),
    max_batches: int | None = typer.Option(None, "--max-batches"),
) -> None:
    if replay is not None:
        # Replay requires a manifest to reconstruct the signer + chain context;
        # fall back to the conventional location if not explicitly supplied.
        resolved_manifest = manifest_path or (replay.parent / "run-manifest.json")
        if not resolved_manifest.exists():
            typer.echo(
                f"--replay requires a manifest; none found at {resolved_manifest}. "
                "Pass --manifest to point at the run manifest.",
                err=True,
            )
            raise typer.Exit(code=5)
        exit_code = replay_run(
            journal_path=replay,
            rpc_url=rpc_url,
            manifest_path=resolved_manifest,
        )
        raise typer.Exit(code=exit_code)

    if target_yaml is None:
        typer.echo("--target-yaml is required when not running --replay", err=True)
        raise typer.Exit(code=2)
    if not target_yaml.exists():
        typer.echo(f"target.yaml not found at {target_yaml}", err=True)
        raise typer.Exit(code=2)

    target = load_target(target_yaml)
    # Live-reload watcher: stat target.yaml once per batch and adopt edits
    # immediately. The CLI is the only place that knows the path is real
    # (``deps``-injected runs may be using synthetic in-memory targets), so
    # the watcher is constructed here.
    watcher = LiveTargetWatcher(target_yaml, initial=target)
    ref_f = load_reference_f(reference_f_path or default_reference_f_path())
    env = EnvInfo(
        genesis_sha256=genesis_sha256,
        plugin_git_sha=plugin_git_sha,
        nethermind_commit_sha=nethermind_commit_sha,
        dotnet_runtime_major=dotnet_runtime_major,
        cpu_arch=platform.machine(),
    )
    lifecycle_run(
        target=target,
        target_watcher=watcher,
        state_dir=state_dir,
        rpc_url=rpc_url,
        sensor_rpc_url=sensor_rpc_url or os.environ.get("NODE_RPC"),
        jwt_path=jwt_path,
        reference_f=ref_f,
        env=env,
        max_batches=max_batches,
    )


if __name__ == "__main__":
    app()
