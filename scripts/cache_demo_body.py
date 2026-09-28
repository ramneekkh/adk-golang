#!/usr/bin/env python3
"""Builds a /v1/responses body whose system prompt is an operating manual
large enough (>1024 tokens) for Vertex explicit caching. A fresh revision
stamp makes each demo run start with a cache MISS."""
import json, re, time

src = open("internal/agents/rag/agent.go").read()
ids = re.findall(r'"(doc\d+)":', src)
clauses = [
    f"Section {i}: When a customer asks about topic {d}, cite policy document {d}, answer only "
    f"from retrieved documents, never invent fees or rates, keep personal data as placeholders, "
    f"and escalate disputes to a human agent."
    for i, d in enumerate(ids * 4, 1)
]
manual = "Krungthai Virtual Assistant operating manual. " + " ".join(clauses) + f" (revision {time.time_ns()})"
print(json.dumps({
    "model": "gemini-2.5-flash",
    "instructions": "You are the Krungthai virtual assistant. Answer from the operating manual in the conversation.",
    # Turn 1-2: long reference document (cached via breakpoint after_turn:2); turn 3: the question.
    "input": [
        {"role": "user", "content": [{"type": "input_text", "text": manual}]},
        {"role": "assistant", "content": [{"type": "output_text", "text": "Understood. I will follow the operating manual."}]},
        {"role": "user", "content": [{"type": "input_text", "text": "ค่าธรรมเนียมบัตรเดบิตเท่าไหร่"}]},
    ],
}))
