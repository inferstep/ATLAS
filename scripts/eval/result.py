"""What one session in either arm ended as."""
from __future__ import annotations

from dataclasses import dataclass

COMPLETED = "completed"


@dataclass
class ArmResult:
    status: str        # completed, incomplete, stopped, timed_out, failed, no_terminal
    reason: str
    wall_s: float
    turns: int = 0
    tokens: int = 0
