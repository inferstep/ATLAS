"""Lyapunov cost field C(x): maps embeddings to scalar energy values.

Architecture: ℝ^d → ℝ^512 → ℝ^128 → ℝ^1  (d = the selected model's hidden
dimension, read from the artifacts/server at load)
Activations: SiLU, SiLU, Softplus (ensures positive output)
Total params: ~2.16M (8.3MB FP32)
"""

import torch
import torch.nn as nn


class CostField(nn.Module):
    """Lyapunov cost field C(x) that maps embeddings to energy scalars.

    High energy = bug-prone region.
    Low energy = correct code attractor basin.
    """

    def __init__(self, input_dim: int):
        super().__init__()
        if not isinstance(input_dim, int) or isinstance(input_dim, bool) \
                or input_dim <= 0:
            raise ValueError("input_dim must be a positive integer")
        self.net = nn.Sequential(
            nn.Linear(input_dim, 512),
            nn.SiLU(),
            nn.Linear(512, 128),
            nn.SiLU(),
            nn.Linear(128, 1),
            nn.Softplus(),  # Ensures positive output
        )

    def set_eval_mode(self):
        """Set model to evaluation mode (disables dropout, batchnorm updates)."""
        self.train(False)
        return self

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        """Compute energy C(x).

        Args:
            x: Embedding tensor of shape (batch, input_dim) or (input_dim,).

        Returns:
            Energy scalar(s) of shape (batch, 1) or (1,).
        """
        return self.net(x)
