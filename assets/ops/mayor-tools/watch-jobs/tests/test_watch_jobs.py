"""Exercise the launchd entry points in a temporary city with a fake gc and fake samplers."""
import json
import os
import plistlib
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


JOBS = Path(__file__).resolve().parents[1]
INTERVALS = {"slack-dead-letter": 120, "far-side-inbox-watch": 180, "inbox-watch": 60,
             "events-consumer": 60, "charge-watch": 150, "patrol-sampler": 60}

FAKE_GC = """
import json
import sys
from pathlib import Path

args = sys.argv[1:]
db = json.loads(Path("fixture.json").read_text())
if args == ["events", "--seq"]:
    print(db["head"])
elif args[:2] == ["events", "--watch"]:
    after = int(args[args.index("--after") + 1])
    for e in db["events"]:
        if e["seq"] > after and ("--type" not in args or e["type"] == "mail.sent"):
            print(json.dumps(e))
elif args in (["mail", "inbox", "mayor", "--json"],
              ["mail", "inbox", "citadel/mayor", "--context", "jadegate", "--json"]):
    print(json.dumps({"ok": True, "messages": db["inbox"], "recipient": args[2]}))
elif args[:3] in (["mail", "send", "mayor"], ["session", "submit", "mayor"]):
    if args[0] == "mail":
        assert {"--notify", "--json", "-m", "-s"}.issubset(args)
    else:
        assert args[-2:] == ["--intent", "follow_up"]
    with Path("actions").open("a") as f:
        print(json.dumps(args), file=f)
    if db["fail_for"] and db["fail_for"] in " ".join(args):
        sys.exit(1)
    print(json.dumps({"command": "mail.send", "id": "receipt", "notified": True}))
else:
    sys.exit("unexpected gc arguments: " + repr(args))
"""

FAKE_SAMPLER = """
import json
import sys
from pathlib import Path

job = Path(__file__).stem
interval = {"charge-watch": "150", "patrol-sampler": "60"}[job]
assert sys.argv[1:] == ["levi", "--interval", interval, "--once"]
db = json.loads(Path("fixture.json").read_text())
with Path("samples").open("a") as f:
    print(job, file=f)
print(db["lines"], end="")
if job == "patrol-sampler" and db["sample_rc"] in (0, 1) and db.get("completed", True):
    print("time_utc,latitude,longitude,error")
    print("T,fixture_lat,fixture_lon,fixture")
sys.exit(db["sample_rc"])
"""


class WatchJobs(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.city = Path(temp.name)
        self.gc = self.city / ".gc"
        self.gc.mkdir()
        binary = self.city / "gc"
        binary.write_text(f"#!{sys.executable}\n" + FAKE_GC)
        binary.chmod(0o700)
        self.db = {"head": 10, "events": [], "inbox": [], "fail_for": "", "lines": "", "sample_rc": 0}

    def tick(self, name, *, dry=False, fail=False):
        (self.city / "fixture.json").write_text(json.dumps(self.db))
        result = subprocess.run([sys.executable, "-I", str(JOBS / (name + ".py")),
                                 *(["--dry-run"] if dry else [])], cwd=self.city,
                                env={**os.environ, "PATH": str(self.city)}, capture_output=True, text=True)
        self.assertEqual(result.returncode != 0, fail, result.stderr)
        return result.stdout

    def actions(self):
        path = self.city / "actions"
        return [json.loads(s) for s in path.read_text().splitlines()] if path.exists() else []

    def state(self, key):
        path = self.gc / f"watch-job-{key}.json"
        return json.loads(path.read_text()) if path.exists() else None

    def test_dead_letters(self):
        name = "slack-dead-letter"
        folder = self.gc / "slack/inbound_dead_letter"
        folder.mkdir(parents=True)
        (folder / "old").touch()
        self.tick(name)
        self.assertEqual(self.actions(), [])
        self.assertEqual(self.state(name), ["old"])
        for filename in ["a", "b"]:
            (folder / filename).touch()
        self.assertIn(str(folder / "a"), self.tick(name, dry=True))
        self.assertEqual(self.actions(), [])
        self.db["fail_for"] = str(folder / "b")
        self.tick(name, fail=True)
        self.assertEqual(self.state(name), ["a", "old"])
        self.db["fail_for"] = ""
        self.tick(name)
        self.tick(name)
        self.assertEqual(self.state(name), ["a", "b", "old"])
        self.assertEqual([a[a.index("-m") + 1] for a in self.actions()],
                         [str(folder / f) for f in ["a", "b", "b"]])

    def test_far_side(self):
        name = "far-side-inbox-watch"
        key = "far-side-inbox"
        (self.gc / "469th-far-side-watch.seen").write_text("old\n")
        def msg(mid, date):
            return {"id": mid, "created_at": date, "from": "remote", "subject": "hello"}
        self.db["inbox"] = [msg("old", "2026-11-02T01:00:00Z"), msg("early", "2026-10-30T00:00:00Z")]
        self.tick(name)
        self.assertEqual(self.actions(), [])
        self.assertEqual(self.state(key), ["old"])
        (self.gc / "469th-far-side-watch.seen").write_text("old\nnew\n")
        self.db["inbox"].append(msg("new", "2026-11-03T01:00:00Z"))
        self.assertIn("Jadegate new", self.tick(name, dry=True))
        self.assertEqual(self.actions(), [])
        self.db["fail_for"] = "Far-side mail"
        self.tick(name, fail=True)
        self.assertEqual(self.state(key), ["old"])
        self.db["fail_for"] = ""
        self.tick(name)
        self.tick(name)
        self.assertEqual(self.state(key), ["new", "old"])
        self.assertEqual(len(self.actions()), 2)
        self.assertEqual(self.actions()[0], self.actions()[1])

    def test_inbox(self):
        name = "inbox-watch"
        self.tick(name)
        self.assertEqual(self.state("inbox"), {"ids": [], "seq": 10})
        self.assertEqual(self.actions(), [])
        confirmed = {"command": "mail.send", "id": "notified", "notified": True}
        (self.gc / "watch-job-charge-watch.log").write_text(json.dumps(confirmed) + "\n")
        (self.gc / "slot-mail.log").write_text("date key sent slot-id attempt 1 notify=1\n")
        (self.gc / "watch-notify.log").write_text('{"id":"founder-id","notified":true}\n')
        mids = ["read", "notified", "slot-id", "founder-id", "plain", "plain", "next"]
        for seq, mid in enumerate(mids, 11):
            msg = {"id": mid, "read": mid == "read", "from": "worker", "to": "mayor"}
            self.db["events"].append({"type": "mail.sent", "seq": seq, "payload": {"message": msg}})
            if mid != "read":
                self.db["inbox"].append(msg)
        self.db["head"] = 17
        dry = self.tick(name, dry=True)
        self.assertEqual(dry.count("Would request"), 2)
        self.assertEqual(self.actions(), [])
        self.assertEqual(self.state("inbox")["seq"], 10)
        self.db["fail_for"] = "mail next."
        self.tick(name, fail=True)
        self.assertEqual(self.state("inbox"), {"ids": ["plain"], "seq": 16})
        self.db["fail_for"] = ""
        self.tick(name)
        self.tick(name)
        self.assertEqual(self.state("inbox"), {"ids": ["next", "plain"], "seq": 17})
        self.assertEqual([a[3].split()[4] for a in self.actions()], ["plain.", "next.", "next."])

    def test_lane_transitions(self):
        name = "events-consumer"
        self.tick(name)
        def event(seq, status, kind="task"):
            bead = {"id": "cp-example", "status": status, "issue_type": kind}
            return {"seq": seq, "type": "bead.updated", "payload": {"bead": bead}}
        self.db["events"] = [event(11, "open"), event(12, "in_progress"), event(13, "closed", "message")]
        self.db["head"] = 13
        quiet = self.tick(name)
        self.assertIn("IN_PROGRESS", quiet)
        self.assertIn(" OPEN", quiet)
        self.assertEqual(self.actions(), [])
        self.assertEqual(self.state("events"), {"seq": 13, "terminal": {}})
        self.db["events"] += [event(14, "blocked"), event(15, "blocked"), event(16, "open"),
                              event(17, "blocked"), event(18, "closed"), event(19, "closed")]
        self.db["head"] = 19
        self.assertEqual(self.tick(name, dry=True).count("Would mail"), 3)
        self.assertEqual(self.actions(), [])
        self.db["fail_for"] = "journal event 17"
        self.tick(name, fail=True)
        self.assertEqual(self.state("events")["seq"], 14)
        self.db["fail_for"] = ""
        self.tick(name)
        self.tick(name)
        self.assertEqual(self.state("events"), {"seq": 19, "terminal": {"cp-example": "closed"}})
        self.assertEqual([a[a.index("-s") + 1] for a in self.actions()],
                         ["cp-example BLOCKED"] * 3 + ["cp-example CLOSED"])

    def sampler(self, job, quiet, alarm, recovery):
        tool = self.city / "assets/ops/mayor-tools" / (job + ".py")
        tool.parent.mkdir(parents=True)
        tool.write_text(FAKE_SAMPLER)
        self.db["lines"] = quiet
        self.tick(job)
        before = self.state(job)
        log = self.gc / f"watch-job-{job}.source.log"
        self.assertEqual(before["offset"], log.stat().st_size)
        self.assertNotIn("fixture_lat", log.read_text())
        self.assertEqual(self.actions(), [])
        self.db.update(lines=alarm, fail_for="--notify", sample_rc=int(job == "patrol-sampler"))
        self.tick(job, fail=True)
        self.assertEqual(self.state(job), before)
        self.assertIn(alarm.strip(), self.tick(job, dry=True))
        self.assertEqual(self.state(job), before)
        self.assertEqual((self.city / "samples").read_text().splitlines(), [job, job])
        self.db.update(lines=recovery, fail_for="", sample_rc=0)
        self.tick(job)
        self.db["lines"] = quiet
        self.tick(job)
        expected = [alarm.strip(), alarm.strip()]
        if job == "patrol-sampler":
            expected.append(recovery.strip())
        self.assertEqual([a[a.index("-m") + 1] for a in self.actions()], expected)
        self.assertEqual(self.state(job)["offset"], (self.gc / f"watch-job-{job}.source.log").stat().st_size)

    def test_charge(self):
        self.sampler("charge-watch", "LEVI CHARGE START synthetic\nLEVI CHARGE END synthetic\n",
                     "LEVI NOT READ three polls running (fixture): charging is not being tracked\n",
                     "LEVI read again after 3 failed polls\n")

    def test_patrol(self):
        self.sampler("patrol-sampler", "time_utc,fixture\n2026-11-26T00:00:00Z,ok\n",
                     "LEVI NOT READ three samples running, last at T (fixture): the patrol samples have gaps\n",
                     "LEVI read again at T after 3 unread samples (patrol sampler)\n")
        self.db.update(lines="", sample_rc=1, completed=False)
        self.tick("patrol-sampler", fail=True)
        self.assertEqual(len(self.actions()), 3)

    def test_plists(self):
        files = sorted((JOBS / "plists").glob("*.plist"))
        self.assertEqual(len(files), len(INTERVALS))
        for path in files:
            with path.open("rb") as f:
                plist = plistlib.load(f)
            script = Path(plist["ProgramArguments"][-1])
            self.assertEqual(script.parent, Path(plist["WorkingDirectory"]) / "assets/ops/mayor-tools/watch-jobs")
            self.assertTrue((JOBS / script.name).is_file())
            self.assertEqual(path.stem, "com.houmanoids.citadel.watch-job-" + script.stem)
            self.assertEqual(plist["Label"], path.stem)
            self.assertEqual(plist["StartInterval"], INTERVALS[script.stem])
            self.assertEqual(plist["StandardOutPath"], plist["StandardErrorPath"])
            self.assertTrue(plist["RunAtLoad"])
            self.assertNotIn("KeepAlive", plist)
