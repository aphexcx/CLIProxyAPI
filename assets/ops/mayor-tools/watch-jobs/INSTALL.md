# Installing the staged Citadel watch jobs

Six jobs are staged. Data-room entry still needs a single-poll entry point. Founder gap already has a launchd twin. Each new plist runs one check using the city as its working directory.

## Run and reverse one job

Define the load, stop and log commands in the Mayor's shell on Citadel.

```sh
cd /Users/tailor512/city
jobs="$PWD/assets/ops/mayor-tools/watch-jobs"
domain="gui/$(id -u)"
prefix=com.houmanoids.citadel.watch-job
load_watch() { launchctl bootstrap "$domain" "$jobs/plists/$prefix-$1.plist"; }
stop_watch() { launchctl bootout "$domain/$prefix-$1"; }
watch_log() { tail -n 10 ".gc/watch-job-$1.log"; }
```

Load one row at a time. Run `watch_log NAME` within its interval and `launchctl print "$domain/$prefix-NAME"` to check its last exit code. A timer normally waits between invocations. For each Levi row, **stop the current sampler Monitor immediately before loading its replacement**. Both would write the same fleet records if they overlapped. On a failed check, boot out the job before re-arming that Monitor. Keep the other Monitors available through their first baseline check. Retire the named manifest row only after seeing the expected result.

| Job | Load | First log proof | Monitor row to retire | Rollback |
| --- | --- | --- | --- | --- |
| Slack dead letters | `load_watch slack-dead-letter` | Within 120 s, `slack-dead-letter: N files, 0 new` establishes the current filenames. A new file produces one mail naming its path. | `slack-dead-letter-watch` | `stop_watch slack-dead-letter` |
| Jadegate inbox | `load_watch far-side-inbox-watch` | Within 180 s, `far-side-inbox: N new`. The explicit remote recipient is `citadel/mayor`. The job inherits the Monitor's cutoff and previously seen IDs. | `far-side-inbox-watch` | `stop_watch far-side-inbox-watch` |
| Mayor inbox | `load_watch inbox-watch` | Within 60 s, `inbox-watch: cursor N, 0 new turns` establishes the starting journal sequence. A subsequent unread worker mail should produce one accepted follow-up. | `inbox-watch` | `stop_watch inbox-watch` |
| Lane status | `load_watch events-consumer` | Within 60 s, `events-consumer: cursor N, 0 terminal transitions` establishes the starting sequence. OPEN and IN_PROGRESS appear in the log. A change to BLOCKED or CLOSED produces one mail. | `events-consumer` | `stop_watch events-consumer` |
| Levi charge | `load_watch charge-watch` | Within 150 s of the first completed poll, `charge-watch: N notifications` and a `poll exit=0` line in its source log. Starts, ends and recovery stay local. Three failed polls produce the tool's existing alarm. | `charge-watch-levi` | `stop_watch charge-watch` |
| Levi patrol | `load_watch patrol-sampler` | Within 60 s of the first completed sample, `patrol-sampler: N notifications` and a `row=True` line in its source log. The original sampler appends the fleet CSV. Its third unread sample and its recovery each produce one mail. | `patrol-sampler-levi` | `stop_watch patrol-sampler` |

Bootstrapping from staging registers a job for this login. For automatic registration at later logins, the Mayor can link the verified plist in the user's LaunchAgents directory after acceptance. For example, with `name=inbox-watch`, run `ln -s "$jobs/plists/$prefix-$name.plist" "$HOME/Library/LaunchAgents/$prefix-$name.plist"`. For rollback, boot out the label, remove only that symlink and re-arm the documented Monitor. The sampler for the patrol post retires when the post ends.

## Delivery and dry run

Run `/opt/homebrew/bin/python3 -I "$jobs/NAME.py" --dry-run` from the city directory. All six preview the pending action without sending or advancing their seen file. The two Levi previews read existing local output and never invoke a sampler. For a repeatable check with synthetic input, run `/opt/homebrew/bin/python3 -I -B -m unittest discover -v -s tests` from the watch-jobs directory.

The five mailing jobs use `gc mail send mayor --notify`. Successful sends produce a JSON receipt in the launchd log. A failed command exits nonzero and leaves that event unacknowledged. Earlier successes in the same batch keep their acknowledgements. Each source must still retain the event for a subsequent run to find it. For the two Levi samplers, the ordinary Monitor output and poll result go to private `.gc/watch-job-NAME.source.log` files. A byte offset tracks acknowledged output, so the one-time alarm can be retried even after the robot recovers. Patrol coordinates stay in the original fleet CSV. Keep the source log and cursor together. A replaced or truncated source log with an incompatible cursor needs reconciliation before restart.

The inbox job selects `gc session submit mayor MESSAGE --intent follow_up`. It asks gc for a turn with the existing mail ID and lets gc schedule it around current work. Sending another mail here would feed the same inbox. The job checks the current unread mailbox before submitting and remembers IDs across journal replays. The installed `mail.sent` event and inbox row omit the notify flag. The job can identify exact IDs with positive receipts in the latest 128 KiB of each staged mailer's log, the founder notification log and the slot timer log. A missing or late receipt leaves a possible second wake for a still-unread message. The job favors delivery in that case.

## Watches that retain their Monitor

**Data-room entry.** Keep `dataroom-entry-monitor` running. Its Python tool posts entries and summaries to Slack, then sleeps inside an unconditional loop. It has no single-poll option. A successful poll is usually silent, and a single poll can print multiple errors. Counting output lines cannot establish three consecutive failed polls. A supported one-poll mode that finishes pending session updates and reports poll success or failure would let a timer count the streak. There is no new plist to load or roll back.

**Founder gap.** Keep `founder-gap-watch` during the existing twin's proof window. The existing label is `com.houmanoids.citadel.watch-founder-gap`. It was already loaded on November 26. Inspect it with `launchctl print "$domain/com.houmanoids.citadel.watch-founder-gap"` and check `.gc/watch-founder-gap-twin.log` within 240 s for a completed sweep, including its failure, held, pending and dropped counts. If the twin is stopped, its existing installation procedure enables the label and bootstraps `"$HOME/Library/LaunchAgents/com.houmanoids.citadel.watch-founder-gap.plist"` in `"$domain"`. Rollback is `launchctl bootout "$domain/com.houmanoids.citadel.watch-founder-gap"`.

Complete the twin's existing 24-hour proof from its recorded start. The report under `assets/ops/records/watch-proof` requires at least 20 completed notified rows and zero `UNREAD-NO-TURN` rows for PASS. Collect another day for NOT ENOUGH. Record the design bars and compare the twin with the Monitor for missed founder lines before the Mayor authorizes retirement. A successful launchd exit alone does not cover delivery.
