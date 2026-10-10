# Examples

| | |
|---|---|
| [`watch/`](watch/) | Go: streams events over WebSocket, reconnecting and resuming with `last_event_id`. `IMSGD_KEY=… go run ./examples/watch -url http://mac.local:8642` |
| [`webhook-receiver/`](webhook-receiver/) | Python (standard library only): verifies webhook signatures, de-duplicates by event ID, prints events |
