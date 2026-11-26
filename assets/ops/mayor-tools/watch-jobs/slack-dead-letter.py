"""Report new filenames in the inbound dead-letter directory."""
import argparse
import json
import subprocess
from pathlib import Path


def main(dry_run):
    directory = Path(".gc/slack/inbound_dead_letter")
    seen_file = Path(".gc/watch-job-slack-dead-letter.json")
    names = {p.name for p in directory.iterdir() if p.is_file()}
    seen = set(json.loads(seen_file.read_text())) if seen_file.exists() else names.copy()

    def save():
        if not dry_run:
            tmp = seen_file.with_suffix(".tmp")
            tmp.write_text(json.dumps(sorted(seen)))
            tmp.replace(seen_file)

    new = sorted(names - seen)
    for name in new:
        body = str((directory / name).absolute())
        if dry_run:
            print("Would mail mayor: new Slack dead letter", body)
        else:
            subprocess.run(["gc", "mail", "send", "mayor", "--notify",
                            "--json", "--dedup", f"watch-job:dead-letter:{name}",
                            "-s", "New Slack dead letter", "-m", body], check=True)
        seen.add(name)
        save()
    if not seen_file.exists():
        save()
    print(f"slack-dead-letter: {len(names)} files, {len(new)} new")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dry-run", action="store_true")
    main(parser.parse_args().dry_run)
