"""
Abstract base class for benchmark datasets.
"""

from abc import ABC, abstractmethod
from typing import List, Iterator
from pathlib import Path

from ..models import BenchmarkTask
from ..config import config


class BaseDataset(ABC):
    """
    Abstract base class for benchmark datasets.

    Subclasses must implement:
        - download(): Download the dataset
        - load(): Load and parse the dataset
        - tasks: Property returning list of BenchmarkTask objects
    """

    def __init__(self, cache_dir: Path = None):
        """
        Initialize the dataset.

        Args:
            cache_dir: Directory for caching downloaded files.
                       Defaults to benchmark/datasets/.cache/
        """
        self.cache_dir = cache_dir or config.cache_dir
        self.cache_dir.mkdir(parents=True, exist_ok=True)
        self._tasks: List[BenchmarkTask] = []
        self._loaded = False

    @property
    @abstractmethod
    def name(self) -> str:
        """Dataset name."""
        pass

    @property
    @abstractmethod
    def expected_count(self) -> int:
        """Expected number of tasks in the dataset."""
        pass

    @abstractmethod
    def download(self) -> Path:
        """
        Download the dataset if not already cached.

        Returns:
            Path to the downloaded file.
        """
        pass

    @abstractmethod
    def _parse(self, filepath: Path) -> List[BenchmarkTask]:
        """
        Parse the dataset file into BenchmarkTask objects.

        Args:
            filepath: Path to the dataset file.

        Returns:
            List of BenchmarkTask objects.
        """
        pass

    def load(self) -> "BaseDataset":
        """
        Load the dataset (download if necessary and parse).

        Returns:
            Self for method chaining.
        """
        if self._loaded:
            return self

        filepath = self.download()
        self._tasks = self._parse(filepath)
        self._loaded = True

        # Validate task count
        if len(self._tasks) != self.expected_count:
            raise ValueError(
                f"Expected {self.expected_count} tasks in {self.name}, "
                f"got {len(self._tasks)}"
            )

        return self

    @property
    def tasks(self) -> List[BenchmarkTask]:
        """
        Get all tasks in the dataset.

        Returns:
            List of BenchmarkTask objects.

        Raises:
            RuntimeError: If dataset hasn't been loaded yet.
        """
        if not self._loaded:
            raise RuntimeError(f"Dataset {self.name} not loaded. Call load() first.")
        return self._tasks

    def __iter__(self) -> Iterator[BenchmarkTask]:
        """Iterate over tasks."""
        return iter(self.tasks)

    def __len__(self) -> int:
        """Return number of tasks."""
        return len(self.tasks)

    def __getitem__(self, idx: int) -> BenchmarkTask:
        """Get task by index."""
        return self.tasks[idx]

    def validate(self) -> bool:
        """
        Validate all tasks in the dataset.

        Checks that each task has required fields and valid test code.

        Returns:
            True if all tasks are valid.

        Raises:
            ValueError: If any task is invalid.
        """
        for task in self.tasks:
            if not task.task_id:
                raise ValueError("Task missing task_id")
            if not task.prompt:
                raise ValueError(f"Task {task.task_id} missing prompt")
            if not task.entry_point:
                raise ValueError(f"Task {task.task_id} missing entry_point")
            # stdio tasks use test_inputs/test_outputs instead of test_code
            if task.eval_mode == "stdio":
                if not task.test_inputs or not task.test_outputs:
                    raise ValueError(f"Task {task.task_id} missing test_inputs/test_outputs for stdio mode")
            else:
                if not task.test_code:
                    raise ValueError(f"Task {task.task_id} missing test_code")
            # Canonical solution can be empty for some evaluation modes
        return True

    def summary(self) -> str:
        """
        Generate a summary of the dataset.

        Returns:
            Formatted summary string.
        """
        lines = [
            f"Dataset: {self.name}",
            f"Tasks: {len(self.tasks)}",
            f"Expected: {self.expected_count}",
        ]

        if self._loaded:
            # Count by difficulty if available
            difficulties = {}
            categories = {}
            for task in self.tasks:
                diff = task.difficulty or "unknown"
                cat = task.category or "unknown"
                difficulties[diff] = difficulties.get(diff, 0) + 1
                categories[cat] = categories.get(cat, 0) + 1

            if len(difficulties) > 1 or "unknown" not in difficulties:
                lines.append(f"Difficulties: {difficulties}")
            if len(categories) > 1 or "unknown" not in categories:
                lines.append(f"Categories: {categories}")

        return "\n".join(lines)
