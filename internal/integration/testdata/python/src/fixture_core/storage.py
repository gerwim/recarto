from abc import ABC, abstractmethod
from typing import Generic, TypeVar

T = TypeVar("T")


class Repository(ABC, Generic[T]):  ## def class Repository
    @abstractmethod  ## annot abstractmethod on Repository.save
    def save(self, item: T) -> int: ...  ## def method Repository.save

    @abstractmethod  ## annot abstractmethod on Repository.find
    def find(self, key: int) -> T | None: ...  ## def method Repository.find


class InMemoryRepository(Repository[T]):  ## def class InMemoryRepository; ref Repository; impl InMemoryRepository of Repository
    def __init__(self) -> None:  ## def method InMemoryRepository.__init__
        self.items: dict[int, T] = {}

    def save(self, item: T) -> int:  ## def method InMemoryRepository.save
        key = len(self.items) + 1
        self.items[key] = item
        return key

    def find(self, key: int) -> T | None:  ## def method InMemoryRepository.find
        return self.items.get(key)


def first[U](items: list[U]) -> U:  ## def function first
    return items[0]
