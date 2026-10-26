import functools
import math
from dataclasses import dataclass
from typing import Optional


@dataclass(frozen=True)  ## annot dataclass on Point
class Point:  ## def class Point
    x: float
    y: float

    def distance_to(self, other: "Point") -> float:  ## def method Point.distance_to; ref Point
        return math.hypot(self.x - other.x, self.y - other.y)

    @staticmethod  ## annot staticmethod on Point.origin
    def origin() -> "Point":  ## def method Point.origin; ref Point
        return Point(0.0, 0.0)  ## ref Point; call Point.origin -> Point


type Pair = tuple[Point, Point]  ## def type Pair; ref Point


def midpoint(pair: Pair) -> Point:  ## def function midpoint; ref Pair; ref Point
    a, b = pair
    return Point((a.x + b.x) / 2, (a.y + b.y) / 2)  ## ref Point; call midpoint -> Point


@functools.lru_cache(maxsize=None)  ## annot lru_cache on span
def span(start: Optional[Point] = None) -> float:  ## def function span; ref Point
    if start is None:
        return 0.0
    return start.distance_to(Point.origin())  ## ref distance_to; ref Point; ref origin; call span -> Point.distance_to; call span -> Point.origin


class Grid:  ## def class Grid
    class Cell:  ## def class Grid.Cell
        def neighbors(self) -> int:  ## def method Grid.Cell.neighbors
            return 4

    def cell(self) -> "Grid.Cell":  ## def method Grid.cell; ref Grid; ref Cell
        return Grid.Cell()  ## ref Grid; ref Cell; call Grid.cell -> Grid.Cell
