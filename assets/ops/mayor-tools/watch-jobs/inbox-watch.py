"""Request a mayor turn for each new journal mail ID that is still unread."""
import argparse
import json
import re
import subprocess
from pathlib import Path


def confirmed_notifications():
    """Use actual send receipts. Two senders may use the same subject."""
    confirmed = set()
    names = ["watch-job-" + name for name in ("slack-dead-letter", "far-side-inbox-watch",
             "events-consumer", "charge-watch", "patrol-sampler")]
    for name in [*names, "watch-notify", "slot-mail"]:
        path = Path(".gc") / (name + ".log")
        if not path.exists():
            continue
        with path.open("rb") as log:
            # Each timer wakes on new events. Limit work on large historical logs.
            end = log.seek(0, 2)
            log.seek(max(0, end - 131072))
            for raw in log:
                if name == "slot-mail":
                    match = re.search(rb" sent ([\w-]+) attempt \d+ notify=1\s*$", raw)
                    if match:
                        confirmed.add(match[1].decode("ascii"))
                elif raw.startswith(b"{"):
                    try:
                        receipt = json.loads(raw)
                    except (ValueError, UnicodeDecodeError):
                        continue
                    if (isinstance(receipt, dict) and receipt.get("notified") is True and
                            (name == "watch-notify" or receipt.get("command") == "mail.send")):
                        confirmed.add(receipt.get("id") or (receipt.get("message") or {}).get("id"))
    return confirmed


def main(dry_run):
    path = Path(".gc/watch-job-inbox.json")
    head = int(subprocess.check_output(["gc", "events", "--seq"]))
    state = json.loads(path.read_text()) if path.exists() else {"seq": head, "ids": []}
    if state["seq"] > head:
        raise ValueError("journal head moved behind inbox cursor; inspect journal before rebaselining")

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
                                         "--timeout", "2s", "--type", "mail.sent",
                                         "--payload-match", "message.to=mayor"], text=True)
        events = sorted((json.loads(line) for line in delta.splitlines()), key=lambda e: e["seq"])
        if events:
            reply = json.loads(subprocess.check_output(["gc", "mail", "inbox", "mayor", "--json"]))
            if reply["ok"] is not True or reply["recipient"] != "mayor":
                raise ValueError("unexpected local mailbox")
            unread = {m["id"] for m in reply["messages"] or [] if not m["read"]}
            notified = confirmed_notifications()
            ids = set(state["ids"]) & unread
            for event in events:
                seq = event["seq"]
                if seq <= state["seq"]:
                    continue
                message = event["payload"]["message"]
                mid = message["id"]
                if (message["to"] == "mayor" and mid in unread and mid not in ids
                        and mid not in notified):
                    body = f"New unread mayor mail {mid}. Read it with gc mail read {mid}."
                    if dry_run:
                        print("Would request a mayor turn:", body)
                    else:
                        subprocess.run(["gc", "session", "submit", "mayor", body,
                                        "--intent", "follow_up"], check=True)
                    ids.add(mid)
                    count += 1
                state.update(seq=seq, ids=sorted(ids))
                save()
        else:
            state["seq"] = head
            save()
    print(f"inbox-watch: cursor {state['seq']}, {count} new turns")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dry-run", action="store_true")
    main(parser.parse_args().dry_run)
