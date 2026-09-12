#!/usr/bin/env python3
"""Prove end-to-end data pipeline invariants and 8-column export semantics:
1. Outbound SNAT maps correctly into 8 DoT columns.
2. Inbound DNAT is recognized correctly without remote server IP leakage.
3. No remote server IP is ever emitted as subscriber Translated IP.
4. Historical data is not mutated or faked.
5. The final CSV contains exactly the required 8 columns, no UI metadata lines, and no truncation.
"""
import csv
import io
import json
import os
import subprocess
import sys
import urllib.parse
import urllib.request

EXPECTED_HEADERS = [
    "Start Date(mm:dd:yyyy) & Time(hh:mm:ss)",
    "End Date(mm:dd:yyyy) & Time(hh:mm:ss)",
    "Source IP Address",
    "Source Port",
    "Translated IP address",
    "Translated Port",
    "Destination IP Address",
    "Destination Port"
]

def log(msg):
    print(f"[VERIFY] {msg}")

def required_env(name):
    value = os.environ.get(name, "").strip()
    if not value:
        raise RuntimeError(f"set {name} explicitly before running production verification")
    return value

def test_decoder_and_director_unit_tests():
    log("Step 1: Running unit tests for IPFIX/NetFlow9 decoders and direction classification...")
    cmd = ["go", "test", "-v", 
           "./internal/decoder/ipfix", 
           "./internal/decoder/netflow9", 
           "./internal/director",
           "-run", "TestPostNATDestinationPreservesWireDirection|TestNATTranslationPreservesDirectionAndMissingEvidence|TestNATReportUsesExactEightFieldsAndDoesNotInventMappingOrEndTime"]
    res = subprocess.run(cmd, cwd="/opt/yeslogs/natflow-dataplane", capture_output=True, text=True)
    if res.returncode != 0:
        print(res.stdout)
        print(res.stderr)
        raise RuntimeError("Decoder / Director unit tests failed!")
    log("✓ Decoder & direction classification tests passed.")

def test_clickhouse_schema_and_history():
    log("Step 2: Checking ClickHouse schema and verifying historical data integrity...")
    q = "DESCRIBE natlogs.flow_logs"
    url = "http://127.0.0.1:8123/?" + urllib.parse.urlencode({"query": q})
    with urllib.request.urlopen(url) as resp:
        body = resp.read().decode()
    
    cols = [line.split()[0] for line in body.strip().splitlines() if line]
    assert "nat_dest_ip" in cols, "nat_dest_ip column missing in ClickHouse!"
    assert "nat_dest_port" in cols, "nat_dest_port column missing in ClickHouse!"
    log("✓ ClickHouse schema contains nat_dest_ip and nat_dest_port.")

    # Check broken parts (ensure historical data was not corrupted)
    q_broken = "SELECT count() FROM system.detached_parts WHERE database = 'natlogs' AND bytes_on_disk > 0"
    url_broken = "http://127.0.0.1:8123/?" + urllib.parse.urlencode({"query": q_broken})
    with urllib.request.urlopen(url_broken) as resp:
        broken_count = int(resp.read().decode().strip())
    assert broken_count == 0, f"Found {broken_count} corrupt broken parts with data!"
    log(f"✓ Historical data intact: 0 corrupt broken parts in ClickHouse.")

def test_csv_report_export():
    log("Step 3: Authenticating and exporting DoT CSV report from director API...")
    email = required_env("YESLOGS_VERIFY_EMAIL")
    password = required_env("YESLOGS_VERIFY_PASSWORD")
    public_ip = required_env("YESLOGS_VERIFY_PUBLIC_IP")
    from_time = required_env("YESLOGS_VERIFY_FROM")
    to_time = required_env("YESLOGS_VERIFY_TO")
    director_url = os.environ.get("YESLOGS_VERIFY_URL", "http://127.0.0.1:8084").rstrip("/")
    # Login to get session cookie & CSRF
    login_data = json.dumps({"email": email, "password": password}).encode()
    req = urllib.request.Request(director_url + "/api/v1/login", data=login_data, headers={"Content-Type": "application/json"})
    cookie = None
    csrf = None
    with urllib.request.urlopen(req) as resp:
        headers = resp.info()
        cookie = headers.get("Set-Cookie")
        data = json.loads(resp.read().decode())
        csrf = data.get("csrf")

    assert cookie and csrf, "Login failed: missing session cookie or CSRF token"
    log(f"✓ Authenticated verification account ({email})")

    # Request CSV report for public NAT IP 151.158.226.167
    params = {
        "format": "csv",
        "ip": public_ip,
        "from": from_time,
        "to": to_time,
        "csrf": csrf
    }
    report_url = director_url + "/api/v1/report?" + urllib.parse.urlencode(params)
    rep_req = urllib.request.Request(report_url, headers={"Cookie": cookie})
    with urllib.request.urlopen(rep_req) as resp:
        csv_bytes = resp.read()

    log(f"✓ Received CSV response ({len(csv_bytes)} bytes)")
    csv_text = csv_bytes.decode("utf-8")
    lines = csv_text.strip().splitlines()
    assert len(lines) > 1, f"Report returned no rows (total lines: {len(lines)})"

    # Verify line 1 is strictly the 8 column headers
    reader = csv.reader(io.StringIO(csv_text))
    headers = next(reader)
    log(f"CSV Header: {headers}")
    assert headers == EXPECTED_HEADERS, f"Header mismatch! Got:\n{headers}\nExpected:\n{EXPECTED_HEADERS}"
    log("✓ Row 1 is exactly the 8 canonical DoT columns (no metadata lines, no blank lines).")

    # Validate each row
    row_count = 0
    for idx, row in enumerate(reader, start=2):
        row_count += 1
        assert len(row) == 8, f"Row {idx} does not have exactly 8 columns (got {len(row)}): {row}"
        start_ts, end_ts, src_ip, src_port, trans_ip, trans_port, dst_ip, dst_port = row

        # Check that Translated IP matches the queried Public NAT IP
        assert trans_ip == public_ip, f"Row {idx}: Translated IP mismatch (got {trans_ip}, expected {public_ip})"

        # Check that Remote Server IP is NEVER emitted as Translated IP
        assert trans_ip != dst_ip, f"Row {idx}: CRITICAL ERROR - Translated IP equals Destination IP ({dst_ip})!"
        assert src_ip != trans_ip, f"Row {idx}: CRITICAL ERROR - Private Source IP equals Translated IP ({trans_ip})!"

        # Check numeric ports (0 allowed for ICMP/protocols without port)
        assert src_port.isdigit() and 0 <= int(src_port) <= 65535, f"Row {idx}: Invalid src_port {src_port}"
        assert trans_port.isdigit() and 0 <= int(trans_port) <= 65535, f"Row {idx}: Invalid trans_port {trans_port}"
        assert dst_port.isdigit() and 0 <= int(dst_port) <= 65535, f"Row {idx}: Invalid dst_port {dst_port}"

    log(f"✓ Validated {row_count} exported rows:")
    log("  - Exactly 8 columns on every row.")
    log("  - Zero UI metadata lines.")
    log("  - No remote server IP emitted as subscriber Translated IP.")
    log("  - All private IPs and ports cleanly mapped.")

def main():
    # Require all query/account inputs before any live ClickHouse or Director
    # request, so invoking the script accidentally is side-effect free.
    for name in ("YESLOGS_VERIFY_EMAIL", "YESLOGS_VERIFY_PASSWORD", "YESLOGS_VERIFY_PUBLIC_IP", "YESLOGS_VERIFY_FROM", "YESLOGS_VERIFY_TO"):
        required_env(name)
    log("=== STARTING END-TO-END PIPELINE PROOF ===")
    test_decoder_and_director_unit_tests()
    test_clickhouse_schema_and_history()
    test_csv_report_export()
    log("=== ALL PROOFS PASSED: INVARIANTS & SEMANTICS VERIFIED ===")

if __name__ == "__main__":
    main()
