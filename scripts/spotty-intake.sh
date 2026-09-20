#!/bin/bash
# Simulate a spotty connection to the Datadog logs intake by randomly dropping
# outbound packets to it. Scoped to the intake IPs on 443 only, so SSH, metrics
# and profile uploads are untouched. TCP retransmits stall the sender, the logs
# pipeline backs up, and the agent's "missed log bytes" health check should fire.
#
#   sudo ./spotty-intake.sh on [drop_pct] [host]   # default 25% to agent-http-intake.logs.<site>
#   sudo ./spotty-intake.sh off
#   sudo ./spotty-intake.sh status
set -euo pipefail
CHAIN=AOC_SPOTTY
PCT=${2:-25}
INTAKE=${3:-agent-http-intake.logs.${DD_SITE:-datadoghq.com}}

command -v iptables >/dev/null || dnf install -y -q iptables-nft

case "${1:-status}" in
  on)
    iptables -N $CHAIN 2>/dev/null || iptables -F $CHAIN
    iptables -C OUTPUT -j $CHAIN 2>/dev/null || iptables -I OUTPUT -j $CHAIN
    ips=$(getent ahostsv4 "$INTAKE" | awk '{print $1}' | sort -u)
    [ -n "$ips" ] || { echo "could not resolve $INTAKE"; exit 1; }
    for ip in $ips; do
      iptables -A $CHAIN -p tcp -d "$ip" --dport 443 \
        -m statistic --mode random --probability "0.$(printf '%02d' "$PCT")" -j DROP
    done
    echo "dropping ${PCT}% of outbound packets to $INTAKE ($(echo $ips | tr '\n' ' '))"
    ;;
  off)
    iptables -D OUTPUT -j $CHAIN 2>/dev/null || true
    iptables -F $CHAIN 2>/dev/null && iptables -X $CHAIN 2>/dev/null || true
    echo "packet loss removed"
    ;;
  status)
    iptables -L $CHAIN -v -n 2>/dev/null || echo "off"
    ;;
  *) echo "usage: $0 on [drop_pct] [intake_host] | off | status"; exit 2 ;;
esac
