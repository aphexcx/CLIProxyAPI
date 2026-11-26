"""Run the existing charging watcher once and notify from its output log."""
import argparse
import json
import os
import subprocess
import sys
from pathlib import Path


def main(dry_run):
    log = Path(".gc/watch-job-charge-watch.source.log")
    seen = Path(".gc/watch-job-charge-watch.json")
    tool = Path("assets/ops/mayor-tools/charge-watch.py")
    if not dry_run:
        command = [sys.executable, "-I", str(tool), "levi", "--interval", "150", "--once"]
        with log.open("ab", buffering=0) as output, subprocess.Popen(command, stdout=subprocess.PIPE) as proc:
            for line in proc.stdout:
                # Omit the state dump added by --once. The original Monitor
                # lines and poll result are sufficient to diagnose the job.
                if not line.startswith(b"state: "):
                    output.write(line)
            rc = proc.wait()
            output.write(f"poll exit={rc}\n".encode())
    elif not log.exists():
        print("charge-watch: no recorded output")
        return

    def save(offset, inode):
        if not dry_run:
            tmp = seen.with_suffix(".tmp")
            tmp.write_text(json.dumps({"offset": offset, "inode": inode}))
            tmp.replace(seen)

    count = 0
    with log.open("rb") as stream:
        info = os.fstat(stream.fileno())
        if seen.exists():
            cursor = json.loads(seen.read_text())
            if cursor["inode"] != info.st_ino or cursor["offset"] > info.st_size:
                raise ValueError("source log replaced or truncated; reconcile its cursor before restarting")
            stream.seek(cursor["offset"])
        for line in stream:
            if line.startswith(b"LEVI NOT READ three polls running ("):
                body = line.decode("utf-8", errors="replace").rstrip()
                if dry_run:
                    print("Would mail mayor:", body)
                else:
                    subprocess.run(["gc", "mail", "send", "mayor", "--notify",
                                    "--json", "-s", "Levi charge watch alarm",
                                    "-m", body, "--dedup",
                                    f"charge-watch:{info.st_ino}:{stream.tell()}"], check=True)
                save(stream.tell(), info.st_ino)
                count += 1
        save(stream.tell(), info.st_ino)
    print(f"charge-watch: {count} notifications, output checked through byte {info.st_size}")
    if not dry_run and rc:
        raise subprocess.CalledProcessError(rc, command)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dry-run", action="store_true")
    os.umask(0o077)
    main(parser.parse_args().dry_run)
