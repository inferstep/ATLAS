"""chat_completion: the request it sends to the model server, and what it reads from the answer.

No server: httpx.post answers from a fixed reply, and what each call was sent is kept.
"""
import pytest

from stages import llm_client


class _Reply:
    def __init__(self, data, status):
        self.data, self.status = data, status

    def raise_for_status(self):
        if self.status >= 400:
            raise RuntimeError(f"HTTP {self.status}")

    def json(self):
        return self.data


def _answer(monkeypatch, data, status=200):
    """httpx.post answers with data; returns what each call was sent."""
    sent = []

    def post(url, json, timeout, headers):
        sent.append({"url": url, "body": json, "timeout": timeout, "headers": headers})
        return _Reply(data, status)

    monkeypatch.setattr(llm_client, "HAS_HTTPX", True)
    monkeypatch.setattr(llm_client.httpx, "post", post)
    monkeypatch.setattr(llm_client, "_service_token", lambda: "the-token")
    return sent


def test_the_request_carries_the_messages_the_settings_and_the_token(monkeypatch):
    sent = _answer(monkeypatch, {})
    llm_client.chat_completion("http://llm", user="hi", system="be brief", temperature=0.3, max_tokens=99, seed=0,
                               enable_thinking=True, want_logprobs=True, timeout=5.0, extra_body={"top_k": 5})
    assert sent == [{
        "url": "http://llm/v1/chat/completions",
        "body": {"messages": [{"role": "system", "content": "be brief"}, {"role": "user", "content": "hi"}],
                 "temperature": 0.3, "max_tokens": 99, "stream": False,
                 "chat_template_kwargs": {"enable_thinking": True},
                 "seed": 0, "logprobs": True, "top_logprobs": 1, "top_k": 5},
        "timeout": 5.0,
        "headers": {"Authorization": "Bearer the-token"},
    }]


@pytest.mark.parametrize("system", [None, ""], ids=["no-system-prompt", "an-empty-system-prompt"])
def test_without_a_system_prompt_or_a_user_one_empty_user_message_is_sent(monkeypatch, system):
    sent = _answer(monkeypatch, {})
    llm_client.chat_completion("http://llm", system=system)
    assert sent[0]["body"]["messages"] == [{"role": "user", "content": ""}]
    assert "seed" not in sent[0]["body"]
    assert "logprobs" not in sent[0]["body"]


def test_an_http_error_is_raised(monkeypatch):
    _answer(monkeypatch, {}, status=500)
    with pytest.raises(RuntimeError, match="HTTP 500"):
        llm_client.chat_completion("http://llm", user="hi")


@pytest.mark.parametrize("data, content, reasoning, tokens", [
    pytest.param({"choices": [{"message": {"content": "answer", "reasoning_content": "why"}}],
                  "usage": {"completion_tokens": 7}}, "answer", "why", 7, id="content-reasoning-and-tokens"),
    pytest.param({"choices": [{"message": {"content": "", "reasoning_content": "the answer"}}]},
                 "the answer", "the answer", 0, id="the-reasoning-when-there-is-no-content"),
    pytest.param({"choices": [{"message": {"content": " \n", "reasoning_content": "the answer"}}]},
                 "the answer", "the answer", 0, id="the-reasoning-when-the-content-is-blank"),
    pytest.param({"choices": [{"message": {"content": "<think>plan</think>answer"}}]},
                 "answer", "", 0, id="leaked-reasoning-is-removed"),
    pytest.param({}, "", "", 0, id="an-answer-without-choices"),
])
def test_the_reply_is_read_from_the_first_choice(monkeypatch, data, content, reasoning, tokens):
    _answer(monkeypatch, data)
    got = llm_client.chat_completion("http://llm", user="hi")
    assert (got["content"], got["reasoning"], got["tokens"]) == (content, reasoning, tokens)


@pytest.mark.parametrize("want_logprobs, logprobs", [
    pytest.param(True, [-0.5], id="asked"),
    pytest.param(False, [], id="not-asked"),
])
def test_logprobs_are_read_only_when_asked(monkeypatch, want_logprobs, logprobs):
    _answer(monkeypatch, {"choices": [{"message": {"content": "a"}, "logprobs": {"content": [{"logprob": -0.5}]}}]})
    assert llm_client.chat_completion("http://llm", user="hi", want_logprobs=want_logprobs)["logprobs"] == logprobs
