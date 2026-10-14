"""Internal service-auth token (lens side).

Reads the per-installation token from ATLAS_SERVICE_TOKEN_FILE
(default /run/atlas-secrets/service-token, the read-only compose
mount). Empty => auth disabled: inbound requests are accepted and
outbound requests carry no header — the pre-token behavior. The token
value is never logged.

install_urllib_opener() covers every urllib.request call site in the
package (embedding extraction, identity probe) with one opener;
httpx call sites merge auth_headers() explicitly. The token goes only
to the model server's addresses, LLAMA_URL and LLAMA_EMBED_URL.
"""

import os
import urllib.parse
import urllib.request


def _load() -> str:
    path = os.environ.get("ATLAS_SERVICE_TOKEN_FILE",
                          "/run/atlas-secrets/service-token")
    try:
        with open(path) as fh:
            return fh.read().strip()
    except FileNotFoundError:
        return ""
    except OSError as error:
        # The file is there but cannot be read. Without a token the service
        # would check nothing, so it stops and says why.
        raise SystemExit(f"the service token file {path} cannot be read ({error.strerror}), so the lens "
                         f"stops rather than run with no check. It runs as user {os.getuid()}: make the file readable "
                         f"for that user.") from error


SERVICE_TOKEN = _load()


def auth_headers() -> dict:
    if not SERVICE_TOKEN:
        return {}
    return {"Authorization": f"Bearer {SERVICE_TOKEN}"}


def _origin(url: str) -> tuple:
    """(scheme, host, port) of an address; the port is 80 or 443 when the address leaves it out."""
    parts = urllib.parse.urlsplit(url)
    return parts.scheme.lower(), (parts.hostname or "").lower(), parts.port or (443 if parts.scheme.lower() == "https" else 80)


def _stack_origins() -> set:
    """The addresses that the lens calls, read as embedding_extractor reads them: the model server's."""
    llama = os.environ.get("LLAMA_URL", "http://llama-server:8080")
    return {_origin(llama), _origin(os.environ.get("LLAMA_EMBED_URL", llama))}


class _TokenForTheStackOnly(urllib.request.HTTPRedirectHandler):
    """The token goes to the stack's own addresses and to no other.

    urllib runs http_request for each request, also for each step of a
    redirect, so a redirect to another address gets no token from here.
    A redirect to another origin also loses an Authorization that the
    caller set itself (model_headers() does), which urllib would copy.
    """

    def http_request(self, request):
        if _origin(request.full_url) in _stack_origins() and not request.has_header("Authorization"):
            request.add_unredirected_header("Authorization", f"Bearer {SERVICE_TOKEN}")
        return request

    https_request = http_request

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        new = super().redirect_request(req, fp, code, msg, headers, newurl)
        if new is not None and _origin(newurl) != _origin(req.full_url):
            new.remove_header("Authorization")
        return new


def install_urllib_opener() -> None:
    """Global opener so bare urlopen()/Request sites send the token to
    the model server. A request that sets its own Authorization keeps it.
    """
    if not SERVICE_TOKEN:
        return
    urllib.request.install_opener(urllib.request.build_opener(_TokenForTheStackOnly()))
