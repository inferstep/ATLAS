"""The V3 service sends its service token only to the stack's addresses (llama, lens, sandbox), also after a redirect."""
import http.server
import threading
import urllib.request

import adapters
from stages import llm_client


def stand_in(name, seen, moved_to=""):
    """A local server that writes down the Authorization of each request in `seen`. A path that starts with /moved
    is sent on to `moved_to` + /after + the path. A POST gets the answer of a model server to a chat completion."""
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            seen[name + self.path] = self.headers.get("Authorization", "")
            self.rfile.read(int(self.headers["Content-Length"]))
            answer = b'{"choices": [{"message": {"content": "ok"}}]}'
            self.send_response(200)
            self.send_header("Content-Length", str(len(answer)))
            self.end_headers()
            self.wfile.write(answer)

        def do_GET(self):
            seen[name + self.path] = self.headers.get("Authorization", "")
            self.send_response(302 if self.path.startswith("/moved") else 200)
            if self.path.startswith("/moved"):
                self.send_header("Location", f"{moved_to}/after{self.path}")
            self.send_header("Content-Length", "0")
            self.end_headers()

        def log_message(self, *args):
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, f"http://127.0.0.1:{server.server_port}"


def test_the_token_goes_only_to_an_address_of_the_stack_and_not_on_to_another(monkeypatch):
    seen = {}
    other, other_address = stand_in("other", seen)
    llama, llama_address = stand_in("llama", seen, moved_to=other_address)
    # The model server is the stand-in "llama". The other one is on the same host, on another port.
    monkeypatch.setattr(adapters, "INFERENCE_URL", llama_address)
    monkeypatch.setattr(adapters, "SERVICE_TOKEN", "a-stand-in-token")
    monkeypatch.setattr(urllib.request, "_opener", None)  # the global opener of the test goes away after it
    adapters.install_urllib_opener()

    for address in (f"{llama_address}/asked", f"{other_address}/asked", f"{llama_address}/moved"):
        urllib.request.urlopen(address, timeout=10).close()
    # A call to the model server that sets the header itself, as chat_completion does on its path with httpx.
    own_header = urllib.request.Request(f"{llama_address}/moved-with-its-own-header",
                                        headers={"Authorization": "Bearer a-stand-in-token"})
    urllib.request.urlopen(own_header, timeout=10).close()
    # The model call of the pipeline, on its path without httpx: it sets no Authorization itself.
    monkeypatch.setattr(llm_client, "HAS_HTTPX", False)
    llm_client.chat_completion(llama_address, user="a question")
    llama.shutdown()
    other.shutdown()

    assert seen == {
        "llama/asked": "Bearer a-stand-in-token",
        "other/asked": "",
        "llama/moved": "Bearer a-stand-in-token",
        "other/after/moved": "",
        "llama/moved-with-its-own-header": "Bearer a-stand-in-token",
        "other/after/moved-with-its-own-header": "",
        "llama/v1/chat/completions": "Bearer a-stand-in-token",
    }
