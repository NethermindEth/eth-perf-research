"""Typer CLI: `orchestrator [run|--replay PATH]`."""
from __future__ import annotations

import platform
from pathlib import Path

import typer

from .lifecycle import run as lifecycle_run
from .manifest import EnvInfo
from .reference_f import default_reference_f_path, load_reference_f
from .replay import replay as replay_run
from .target import load_target


app = typer.Typer(add_completion=False, help="Bloating feedback-loop orchestrator")


@app.callback(invoke_without_command=True)
def main(
    ctx: typer.Context,
    replay: Path | None = typer.Option(
        None, "--replay", help="Replay a journal; no controller, no sensor."
    ),
    rpc_url: str = typer.Option("http://localhost:8545", "--rpc-url"),
    state_dir: Path = typer.Option(Path("./state"), "--state-dir"),
    target_yaml: Path = typer.Option(Path("./target.yaml"), "--target-yaml"),
    reference_f_path: Path | None = typer.Option(None, "--reference-f"),
    manifest_path: Path | None = typer.Option(None, "--manifest"),
    max_batches: int | None = typer.Option(None, "--max-batches"),
    genesis_sha256: str = typer.Option("", "--genesis-sha256"),
    plugin_git_sha: str = typer.Option("", "--plugin-git-sha"),
    nethermind_commit_sha: str = typer.Option("", "--nethermind-commit-sha"),
    dotnet_runtime_major: int = typer.Option(10, "--dotnet-runtime-major"),
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

    if not target_yaml.exists():
        typer.echo(f"target.yaml not found at {target_yaml}", err=True)
        raise typer.Exit(code=2)

    target = load_target(target_yaml)
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
        state_dir=state_dir,
        rpc_url=rpc_url,
        reference_f=ref_f,
        env=env,
        max_batches=max_batches,
    )


if __name__ == "__main__":
    app()
