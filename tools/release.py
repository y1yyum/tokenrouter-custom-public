#!/usr/bin/env python3
"""Prepare, validate, and publish fork release tags without upstream SDK gates."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
from pathlib import Path
import re
import subprocess
import sys

# release.py prepares and publishes canonical vMAJOR.MINOR.PATCH release tags
# for this repository. Unlike upstream tr_release.py, it supports repositories
# with no existing release tags and skips external npm/Go SDK publication gates.

TAG_RE = re.compile(r"^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$")
INITIAL_TAG = "v1.0.0"


@dataclass(frozen=True)
class VersionTag:
    """VersionTag holds a canonical vMAJOR.MINOR.PATCH tag and its sortable tuple."""

    name: str
    major: int
    minor: int
    patch: int

    @property
    def key(self) -> tuple[int, int, int]:
        return (self.major, self.minor, self.patch)


@dataclass(frozen=True)
class ReleaseContext:
    """ReleaseContext records the resolved baseline tag, target tag, and commit range."""

    previous: VersionTag | None
    next_tag: str
    head: str
    commit_count: int
    previous_commit: str | None


def run_git(
    args: list[str],
    *,
    check: bool = True,
    input_text: str | None = None,
    cwd: Path | None = None,
) -> subprocess.CompletedProcess[str]:
    """run_git executes a git command and fails with stderr/stdout diagnostics on error."""
    result = subprocess.run(
        ["git", *args],
        cwd=cwd,
        check=False,
        input=input_text,
        text=True,
        encoding="utf-8",
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if check and result.returncode != 0:
        command = "git " + " ".join(args)
        sys.stderr.write(f"{command} failed:\n{result.stderr or result.stdout}\n")
        raise SystemExit(result.returncode)
    return result


def parse_version_tag(raw_version: str) -> VersionTag:
    """parse_version_tag normalizes an optional v prefix and enforces canonical SemVer."""
    version = raw_version.strip()
    if not version.startswith("v"):
        version = f"v{version}"
    match = TAG_RE.fullmatch(version)
    if not match:
        raise argparse.ArgumentTypeError(
            "Version must follow canonical MAJOR.MINOR.PATCH (e.g., 1.0.0 or v1.0.0; no suffix or leading zeros)"
        )
    major, minor, patch = (int(part) for part in match.groups())
    return VersionTag(version, major, minor, patch)


def parse_version_tags(raw_tags: str) -> list[VersionTag]:
    """parse_version_tags extracts canonical vMAJOR.MINOR.PATCH tags from lines."""
    tags: list[VersionTag] = []
    for line in raw_tags.splitlines():
        match = TAG_RE.fullmatch(line.strip())
        if not match:
            continue
        major, minor, patch = (int(part) for part in match.groups())
        tags.append(VersionTag(line.strip(), major, minor, patch))
    return tags


def latest_origin_tag(cwd: Path | None = None) -> VersionTag | None:
    """latest_origin_tag returns the highest canonical v* tag on origin, or None when empty."""
    result = run_git(["ls-remote", "--tags", "--refs", "origin"], cwd=cwd)
    names = [
        line.split("\t", 1)[1].removeprefix("refs/tags/")
        for line in result.stdout.splitlines()
        if "\t" in line
    ]
    tags = parse_version_tags("\n".join(names))
    if not tags:
        return None
    return max(tags, key=lambda tag: tag.key)


def ensure_clean_tracked_tree(cwd: Path | None = None) -> None:
    """ensure_clean_tracked_tree rejects uncommitted edits to tracked repository files."""
    result = run_git(["status", "--porcelain"], cwd=cwd)
    dirty = [line for line in result.stdout.splitlines() if line and not line.startswith("?? ")]
    if dirty:
        sys.stderr.write("Resolve uncommitted changes to tracked files before releasing:\n")
        sys.stderr.write("\n".join(dirty) + "\n")
        raise SystemExit(1)


def ensure_branch_aligned(cwd: Path | None = None) -> None:
    """ensure_branch_aligned rejects local branches that diverged from their upstream."""
    branch = run_git(["branch", "--show-current"], cwd=cwd).stdout.strip()
    if not branch:
        return
    upstream = run_git(
        ["rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"],
        check=False,
        cwd=cwd,
    )
    if upstream.returncode != 0:
        return
    counts = run_git(
        ["rev-list", "--left-right", "--count", f"{upstream.stdout.strip()}...HEAD"],
        cwd=cwd,
    )
    behind, ahead = (int(part) for part in counts.stdout.split())
    if behind or ahead:
        sys.stderr.write(
            f"Branch {branch} is not aligned with {upstream.stdout.strip()} "
            f"(behind={behind}, ahead={ahead}). Sync before releasing.\n"
        )
        raise SystemExit(1)


def build_release_context(
    requested_version: VersionTag | None = None,
    *,
    cwd: Path | None = None,
) -> ReleaseContext:
    """build_release_context resolves the release tag, baseline commit, and commit count."""
    previous = latest_origin_tag(cwd=cwd)
    if requested_version is not None:
        next_version = requested_version
    elif previous is not None:
        next_version = VersionTag(
            f"v{previous.major}.{previous.minor}.{previous.patch + 1}",
            previous.major,
            previous.minor,
            previous.patch + 1,
        )
    else:
        next_version = parse_version_tag(INITIAL_TAG)

    if previous is not None and next_version.key <= previous.key:
        sys.stderr.write(
            f"Requested version {next_version.name} must be higher than latest origin tag {previous.name}.\n"
        )
        raise SystemExit(1)

    head = run_git(["rev-parse", "HEAD"], cwd=cwd).stdout.strip()
    previous_commit: str | None = None
    if previous is not None:
        previous_commit = run_git(
            ["rev-parse", f"refs/tags/{previous.name}^{{commit}}"],
            cwd=cwd,
        ).stdout.strip()
        ancestry = run_git(
            ["merge-base", "--is-ancestor", previous_commit, head],
            check=False,
            cwd=cwd,
        )
        if ancestry.returncode != 0:
            sys.stderr.write(
                f"Latest origin tag {previous.name} is not an ancestor of HEAD; resolve release branch history first.\n"
            )
            raise SystemExit(1)
        count_raw = run_git(["rev-list", "--count", f"{previous_commit}..{head}"], cwd=cwd).stdout.strip()
    else:
        count_raw = run_git(["rev-list", "--count", head], cwd=cwd).stdout.strip()

    local_exists = run_git(
        ["rev-parse", "-q", "--verify", f"refs/tags/{next_version.name}"],
        check=False,
        cwd=cwd,
    ).returncode == 0
    if local_exists:
        sys.stderr.write(f"Local tag {next_version.name} already exists.\n")
        raise SystemExit(1)

    remote_exists = run_git(
        ["ls-remote", "--exit-code", "--tags", "origin", next_version.name],
        check=False,
        cwd=cwd,
    ).returncode == 0
    if remote_exists:
        sys.stderr.write(f"Remote tag {next_version.name} already exists on origin.\n")
        raise SystemExit(1)

    return ReleaseContext(previous, next_version.name, head, int(count_raw), previous_commit)


def draft_release_notes(context: ReleaseContext, *, cwd: Path | None = None) -> str:
    """draft_release_notes generates a baseline Markdown changelog from recent commits."""
    if context.previous_commit:
        rev_range = f"{context.previous_commit}..{context.head}"
        log_args = ["log", "--no-merges", "--pretty=format:- %s", rev_range]
    else:
        log_args = ["log", "--no-merges", "--pretty=format:- %s", "-n", "30", context.head]
    subjects = run_git(log_args, cwd=cwd).stdout.strip()
    if not subjects:
        subjects = f"- Maintenance release {context.next_tag}."
    return f"Release {context.next_tag}.\n\n## Changes\n\n{subjects}\n"


def read_release_notes(source: str) -> str:
    """read_release_notes loads and validates non-empty Markdown release notes."""
    raw = sys.stdin.read() if source == "-" else Path(source).read_text(encoding="utf-8")
    notes = raw.strip()
    if not notes:
        sys.stderr.write("Release notes must not be empty.\n")
        raise SystemExit(1)
    lines = notes.splitlines()
    first_heading = next((i for i, line in enumerate(lines) if line.startswith("## ")), None)
    if first_heading is None or not "\n".join(lines[:first_heading]).strip():
        sys.stderr.write("Release notes need a summary sentence followed by at least one '## ' heading.\n")
        raise SystemExit(1)
    if not any(line.startswith("- ") for line in lines[first_heading + 1 :]):
        sys.stderr.write("Release notes need at least one '- ' list item under a section.\n")
        raise SystemExit(1)
    return notes


def create_and_push_tag(
    context: ReleaseContext,
    notes: str,
    *,
    dry_run: bool = False,
    cwd: Path | None = None,
) -> None:
    """create_and_push_tag writes an annotated tag with verbatim notes and pushes it to origin."""
    version = context.next_tag.removeprefix("v")
    message = f"TokenRouter {version}\n\n{notes}\n"
    if dry_run:
        return

    run_git(
        ["tag", "-a", "--cleanup=verbatim", context.next_tag, context.head, "-F", "-"],
        input_text=message,
        cwd=cwd,
    )
    tag_type = run_git(["cat-file", "-t", context.next_tag], cwd=cwd).stdout.strip()
    tag_body = run_git(["tag", "--list", "--format=%(contents:body)", context.next_tag], cwd=cwd).stdout.strip()
    if tag_type != "tag" or tag_body != notes:
        run_git(["tag", "-d", context.next_tag], check=False, cwd=cwd)
        sys.stderr.write("Annotated tag body verification failed; deleted local tag.\n")
        raise SystemExit(1)

    push = run_git(["push", "origin", context.next_tag], check=False, cwd=cwd)
    if push.returncode != 0:
        run_git(["tag", "-d", context.next_tag], check=False, cwd=cwd)
        sys.stderr.write(f"Pushing {context.next_tag} failed; deleted local tag:\n{push.stderr or push.stdout}\n")
        raise SystemExit(push.returncode)


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    """parse_args parses prepare/publish release commands."""
    parser = argparse.ArgumentParser(description="Prepare or publish a fork TokenRouter release tag.")
    parser.add_argument("--prepare", action="store_true", help="Inspect origin tags and print release range.")
    parser.add_argument("--version", type=parse_version_tag, help="Target version (e.g., 1.0.0 or v1.0.0).")
    parser.add_argument("--notes-file", metavar="PATH", help="Reviewed Markdown release notes file (or '-' for stdin).")
    parser.add_argument("--draft-notes", metavar="PATH", help="Write drafted Markdown release notes to PATH during --prepare.")
    parser.add_argument("--expected-head", help="Require HEAD to match this SHA before publishing.")
    parser.add_argument("--dry-run", action="store_true", help="Validate tag creation without mutating Git or pushing.")
    args = parser.parse_args(argv)
    if not args.prepare and not args.notes_file:
        parser.error("publication requires --notes-file PATH (or pass --prepare to inspect/draft)")
    return args


def main(argv: list[str] | None = None) -> int:
    """main coordinates pre-release validation, draft notes generation, and tag publishing."""
    args = parse_args(argv)
    ensure_clean_tracked_tree()
    run_git(["fetch", "origin", "--tags"])
    ensure_branch_aligned()

    context = build_release_context(args.version)
    if args.prepare:
        if args.draft_notes:
            Path(args.draft_notes).write_text(draft_release_notes(context), encoding="utf-8")
        print(f"previous_tag={context.previous.name if context.previous else '(none)'}")
        print(f"new_tag={context.next_tag}")
        print(f"commit={context.head}")
        print(f"commit_count={context.commit_count}")
        print(
            f"change_range={(context.previous_commit + '..' + context.head) if context.previous_commit else context.head}"
        )
        return 0

    if args.expected_head and context.head != args.expected_head:
        sys.stderr.write(f"HEAD changed ({context.head} != {args.expected_head}); rerun --prepare.\n")
        return 1

    notes = read_release_notes(args.notes_file)
    create_and_push_tag(context, notes, dry_run=args.dry_run)
    print(f"new_tag={context.next_tag}")
    print(f"commit={context.head}")
    print(f"dry_run={'true' if args.dry_run else 'false'}")
    print(f"pushed={'false' if args.dry_run else 'true'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
