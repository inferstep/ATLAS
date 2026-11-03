import os
from pydantic import BaseModel


class ServerConfig(BaseModel):
    port: int = 8099
    host: str = "0.0.0.0"


class LlamaConfig(BaseModel):
    base_url: str = os.environ.get("LLAMA_URL", "http://llama-server:8080")


class Config(BaseModel):
    server: ServerConfig = ServerConfig()
    llama: LlamaConfig = LlamaConfig()


config = Config()
