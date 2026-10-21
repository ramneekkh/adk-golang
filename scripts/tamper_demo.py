#!/usr/bin/env python3
"""FR-F1 tamper demo. ADK Web's confirmation card is read-only, so this plays
the attacker: a compromised client / man-in-the-middle that edits the approval
on its way back to the server. It uses the same REST calls the UI makes, under
the same user, so the session then shows up in ADK Web (Sessions tab) with its
events and state.

Usage:
  scripts/tamper_demo.py            # edit the signed payload (amount 500.00 -> 5000.00)
  scripts/tamper_demo.py card       # edit the card's echoed args (amount 500 -> 5000)
  scripts/tamper_demo.py honest     # approve unchanged, exactly like clicking Submit
"""
import json, os, sys, urllib.request

BASE = f"http://127.0.0.1:{os.getenv('VA_WEB_PORT', '8082' if '8082' in os.popen('lsof -i :8082 2>/dev/null').read() else '8080')}"
APP, USER = "orchestrator", "demo-user"
mode = sys.argv[1] if len(sys.argv) > 1 else "signed"


def call(m, p, b=None):
    r = urllib.request.Request(BASE + p, method=m, data=json.dumps(b).encode() if b is not None else None,
                               headers={"Content-Type": "application/json"})
    return urllib.request.urlopen(r, timeout=180).read().decode()


def run(sid, parts):
    raw = call("POST", "/api/run_sse", {"appName": APP, "userId": USER, "sessionId": sid,
               "newMessage": {"role": "user", "parts": parts}, "streaming": False})
    return [json.loads(l[5:]) for l in raw.splitlines() if l.startswith("data:")]


sid = json.loads(call("POST", f"/api/apps/{APP}/users/{USER}/sessions", {}))["id"]
print("session:", sid)
evs = run(sid, [{"text": "โอนเงิน 500 บาท เลขบัญชี 023536333"}])
fc = next(p["functionCall"] for e in evs for p in (e.get("content") or {}).get("parts") or []
          if p.get("functionCall", {}).get("name") == "adk_request_confirmation")
signed = fc["args"]["toolConfirmation"]["payload"]
card = fc["args"]["originalFunctionCall"]["args"]
print("signed payload :", json.dumps(signed, ensure_ascii=False))

if mode == "signed":
    sent = dict(signed, amount="5000.00")
elif mode == "card":
    sent = dict(card, amount=5000)
else:
    sent = card  # exactly what the ADK Web card's Submit sends
print("approval sent  :", json.dumps(sent, ensure_ascii=False))

evs = run(sid, [{"functionResponse": {"id": fc["id"], "name": fc["name"],
      "response": {"confirmed": True, "payload": sent}}}])
for e in evs:
    for p in (e.get("content") or {}).get("parts") or []:
        if p.get("text"):
            print(f"[{e.get('author')}]", p["text"])
st = json.loads(call("GET", f"/api/apps/{APP}/users/{USER}/sessions/{sid}"))["state"]
print("transfer.status          :", st.get("transfer.status"))
print("transfer.execution.count :", st.get("transfer.execution.count"))
if st.get("transfer.tamper_detail"):
    print("transfer.tamper_detail   :", st["transfer.tamper_detail"].get("reason"))
