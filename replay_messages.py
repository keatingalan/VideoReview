#!/usr/bin/env python3
"""Replay saved messages from EventData/events.db to video-server /events."""

import argparse
import json
import sqlite3
import time
import urllib.error
import urllib.request


def replay_messages(db_path, server_url, interval, start_id, limit):
    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    try:
        cur = conn.cursor()
        query = "SELECT id, message FROM messages"
        params = []
        if start_id is not None:
            query += " WHERE id >= ?"
            params.append(start_id)
        query += " ORDER BY id"
        if limit is not None:
            query += " LIMIT ?"
            params.append(limit)
        cur.execute(query, params)

        count = 0
        for row in cur:
            msg_id = row["id"]
            msg_text = row["message"]
            try:
                msg_json = json.loads(msg_text)
            except json.JSONDecodeError as exc:
                print(f"Skipping id={msg_id}: invalid JSON ({exc})")
                continue

            body = json.dumps(msg_json).encode("utf-8")
            req = urllib.request.Request(
                server_url,
                data=body,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            try:
                with urllib.request.urlopen(req, timeout=10) as resp:
                    status = resp.status
                    print(f"sent id={msg_id} status={status}")
            except urllib.error.HTTPError as exc:
                print(f"HTTP error id={msg_id}: {exc.code} {exc.reason}")
                break
            except urllib.error.URLError as exc:
                print(f"Network error id={msg_id}: {exc}")
                break

            count += 1
            time.sleep(interval)

        print(f"Replayed {count} message(s) to {server_url}")
    finally:
        conn.close()


def main():
    parser = argparse.ArgumentParser(description="Replay messages from SQLite events.db to video-server.")
    parser.add_argument(
        "--db",
        default="EventData/events.db",
        help="Path to the events SQLite DB (default: EventData/events.db)",
    )
    parser.add_argument(
        "--url",
        default="http://127.0.0.1:3001/events",
        help="Video server /events endpoint URL (default: http://127.0.0.1:3001/events)",
    )
    parser.add_argument(
        "--interval",
        type=float,
        default=0.5,
        help="Delay between messages in seconds (default: 0.5)",
    )
    parser.add_argument(
        "--start-id",
        type=int,
        default=None,
        help="Optional start ID from messages table",
    )
    parser.add_argument(
        "--limit",
        type=int,
        default=None,
        help="Optional maximum number of messages to replay",
    )
    args = parser.parse_args()

    replay_messages(args.db, args.url, args.interval, args.start_id, args.limit)


if __name__ == "__main__":
    main()
