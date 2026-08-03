#!/usr/bin/env python3
"""Find keys that place on a clock rather than by hand.

Reads the event log and reports fingerprints whose placements are locked to the
cooldown. Prints a comma separated list ready for SSHPLACE_SLOW_KEYS, which puts
them on a longer cooldown rather than throwing them out.

    ./detect-bots.py                      # the live event log on the server
    ./detect-bots.py --hours 12           # look further back
    ./detect-bots.py --loose              # include borderline keys
    ./detect-bots.py path/to/events.jsonl

What it looks for is duty cycle and precision, not volume. A dedicated human and
a bot place at similar *rates*: the busiest real players measured 172 placements
an hour against the bots' 240. What no human does is place every 15.50 seconds
for four hours with a median deviation of ten milliseconds.

Read the output before acting on it. Slowing a real player is worse than missing
a bot, so the default thresholds are deliberately conservative and anything short
of certain lands in the borderline list instead.
"""

import argparse
import collections
import datetime
import json
import statistics
import sys

DEFAULT_LOG = "/var/lib/docker/volumes/sshplace_canvas-data/_data/events.jsonl"


def parse_args():
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("log", nargs="?", default=DEFAULT_LOG,
                   help=f"event log path (default: {DEFAULT_LOG})")
    p.add_argument("--hours", type=float, default=4.0,
                   help="how far back to look (default: 4)")
    p.add_argument("--cooldown", type=float, default=15.0,
                   help="server cooldown in seconds (default: 15)")
    p.add_argument("--loose", action="store_true",
                   help="also include borderline keys in the printed list")
    return p.parse_args()


def load(path, hours):
    events = []
    with open(path) as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                e = json.loads(line)
                e["_t"] = datetime.datetime.fromisoformat(e["t"].replace("Z", "+00:00"))
                events.append(e)
            except (ValueError, KeyError):
                # A torn final line is normal: the log is append-only and may be
                # mid-write. Skipping it beats refusing to run.
                continue
    if not events:
        sys.exit(f"no usable events in {path}")
    cutoff = events[-1]["_t"] - datetime.timedelta(hours=hours)
    return [e for e in events if e["_t"] >= cutoff]


def profile(times, cooldown):
    """Return (n, locked_fraction, mad, span_minutes) for one identity."""
    gaps = [(times[i + 1] - times[i]).total_seconds() for i in range(len(times) - 1)]
    # Drop the idle gaps. Someone who wanders off and comes back is not evidence
    # either way, and leaving them in wrecks every dispersion measure.
    gaps = [g for g in gaps if g < cooldown * 8]
    if len(gaps) < 10:
        return None
    locked = sum(1 for g in gaps if cooldown <= g <= cooldown + 1.0) / len(gaps)
    median = statistics.median(gaps)
    # Median absolute deviation, not stdev: one long pause should not be able to
    # disguise a machine as a human.
    mad = statistics.median([abs(g - median) for g in gaps])
    span = (times[-1] - times[0]).total_seconds() / 60
    return len(times), locked, mad, span


def main():
    args = parse_args()
    events = load(args.log, args.hours)

    by_id = collections.defaultdict(list)
    for e in events:
        by_id[e["id"]].append(e["_t"])

    rows = []
    for identity, times in by_id.items():
        # Only real keys. Clients with no key are identified by network, and a
        # whole network sharing one identity has no meaningful rhythm.
        if not identity.startswith("SHA256:"):
            continue
        p = profile(sorted(times), args.cooldown)
        if p:
            rows.append((identity,) + p)

    certain, borderline = [], []
    for row in rows:
        _, n, locked, mad, span = row
        if locked >= 0.85 and mad <= 0.5 and n >= 100 and span >= 30:
            certain.append(row)
        elif locked >= 0.75:
            borderline.append(row)

    certain.sort(key=lambda r: -r[1])
    borderline.sort(key=lambda r: -r[1])

    total = len(events)
    print(f"window: last {args.hours}h, {total} placements, {len(rows)} active keys\n")

    def show(label, group):
        print(f"{label}: {len(group)}")
        for identity, n, locked, mad, span in group:
            print(f"  n={n:5d}  locked={100 * locked:3.0f}%  MAD={mad:4.2f}s  "
                  f"span={span:4.0f}min  {identity}")
        print()

    show("CLOCK-LOCKED (confident)", certain)
    show("BORDERLINE (excluded unless --loose)", borderline)

    counted = sum(r[1] for r in certain)
    if total:
        print(f"confident keys account for {counted}/{total} = "
              f"{100 * counted / total:.1f}% of placements\n")

    listed = certain + (borderline if args.loose else [])
    if listed:
        print("SSHPLACE_SLOW_KEYS value:")
        print(",".join(r[0] for r in listed))
    else:
        print("nothing to slow down")


if __name__ == "__main__":
    main()
