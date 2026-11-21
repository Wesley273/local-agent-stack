import httpx

c = httpx.Client(base_url="http://127.0.0.1:8080", timeout=10)
h = {"X-API-Key": "admin-key"}
r1 = c.get("/.well-known/agent.json", headers=h)
print("STEP1", r1.status_code, len(r1.content), "conn=", r1.headers.get("connection"))
try:
    r2 = c.post(
        "/a2a/v1/message:send",
        json={"message": {"kind": "message", "messageId": "m1", "role": "user",
                          "parts": [{"kind": "text", "text": "hello"}]}},
        headers=h,
    )
    print("STEP2", r2.status_code, len(r2.content))
except Exception as e:
    print("STEP2 ERR", type(e).__name__, e)
