"""Mail terminal lane status transitions from the city journal."""
import argparse
import json
import subprocess
from pathlib import Path


def lane_status(event):
    kind = event["type"]
    if kind not in {"bead.created", "bead.updated", "bead.closed", "bead.dead_assignee_reopened"}:
        return None
    bead = (event.get("payload") or {}).get("bead") or {}
    bid = bead.get("id") or event.get("subject")
    if not bid or bid.startswith("ci-wisp") or bead.get("issue_type") in {"session", "convoy", "message"}:
        return None
    if {"gc:session", "gc:extmsg-transcript"}.intersection(bead.get("labels") or []):
        return None
    status = bead.get("status") or {"bead.closed": "closed", "bead.dead_assignee_reopened": "open"}.get(kind)
    return (bid, status) if status else None


def main(dry_run):
    path = Path(".gc/watch-job-events.json")
    head = int(subprocess.check_output(["gc", "events", "--seq"]))
    state = json.loads(path.read_text()) if path.exists() else {"seq": head, "terminal": {}}
    if state["seq"] > head:
        raise ValueError("journal head moved behind lane cursor; inspect journal before rebaselining")

    def save():
        if not dry_run:
            tmp = path.with_suffix(".tmp")
            tmp.write_text(json.dumps(state))
            tmp.replace(path)

    if not path.exists():
        save()
    count = 0
    if state["seq"] < head:
        delta = subprocess.check_output(["gc", "events", "--watch", "--after", str(state["seq"]),
                                         "--timeout", "2s"], text=True)
        events = sorted((json.loads(line) for line in delta.splitlines()), key=lambda e: e["seq"])
        for event in events:
            seq = event["seq"]
            if seq <= state["seq"]:
                continue
            row = lane_status(event)
            if row:
                bid, status = row
                print(f"lane {seq} {bid} {status.upper()}")
                if status not in ("closed", "blocked"):
                    state["terminal"].pop(bid, None)
                elif state["terminal"].get(bid) != status:
                    body = f"Lane ticket {bid} is {status.upper()} (journal event {seq})."
                    if dry_run:
                        print("Would mail mayor:", body)
                    else:
                        subprocess.run(["gc", "mail", "send", "mayor", "--notify", "--json",
                                        "--dedup", f"watch-job:lane:{bid}:{seq}",
                                        "-s", f"{bid} {status.upper()}", "-m", body], check=True)
                    state["terminal"][bid] = status
                    count += 1
                    state["seq"] = seq
                    save()
            state["seq"] = seq
        if events:
            save()
    print(f"events-consumer: cursor {state['seq']}, {count} terminal transitions")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dry-run", action="store_true")
    main(parser.parse_args().dry_run)
