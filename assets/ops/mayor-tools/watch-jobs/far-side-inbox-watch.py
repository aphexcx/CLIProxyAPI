"""Report newly observed Jadegate mail addressed to citadel/mayor."""
import argparse
import json
import subprocess
from datetime import datetime
from pathlib import Path


def main(dry_run):
    seen_file = Path(".gc/watch-job-far-side-inbox.json")
    legacy = Path(".gc/469th-far-side-watch.seen")
    if seen_file.exists():
        seen = set(json.loads(seen_file.read_text()))
    else:
        seen = set(legacy.read_text().splitlines()) if legacy.exists() else set()

    def save():
        if not dry_run:
            tmp = seen_file.with_suffix(".tmp")
            tmp.write_text(json.dumps(sorted(seen)))
            tmp.replace(seen_file)

    cutoff = datetime.fromisoformat("2026-11-01T12:36:56+00:00")
    reply = json.loads(subprocess.check_output(
        ["gc", "mail", "inbox", "citadel/mayor", "--context", "jadegate", "--json"]))
    if reply["ok"] is not True or reply["recipient"] != "citadel/mayor":
        raise ValueError("unexpected remote mailbox")
    count = 0
    for msg in sorted(reply["messages"] or [], key=lambda m: m["created_at"]):
        mid = msg["id"]
        if mid in seen or datetime.fromisoformat(msg["created_at"].replace("Z", "+00:00")) <= cutoff:
            continue
        body = f"Jadegate {mid} from {msg['from']}: {msg['subject'][:160]}"
        if dry_run:
            print("Would mail mayor:", body)
        else:
            subprocess.run(["gc", "mail", "send", "mayor", "--notify",
                            "--json", "--dedup", f"watch-job:far-side:{mid}", "-s", f"Far-side mail {mid}",
                            "-m", body], check=True)
        count += 1
        seen.add(mid)
        save()
    if not seen_file.exists():
        save()
    print(f"far-side-inbox: {count} new")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dry-run", action="store_true")
    main(parser.parse_args().dry_run)
