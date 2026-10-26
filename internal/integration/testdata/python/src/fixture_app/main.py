import asyncio
from collections.abc import Callable

import fixture_core.geometry as geo
from fixture_core import Circle, core_version, shapes  ## ref Circle; ref core_version
from fixture_core.shapes import Shape, Square, is_large, total_area  ## ref Shape; ref Square; ref is_large; ref total_area
from fixture_core.storage import InMemoryRepository, Repository, first  ## ref InMemoryRepository; ref Repository; ref first

REGISTRY: list[str] = []


def registered[F: Callable[..., object]](fn: F) -> F:  ## def function registered
    REGISTRY.append(getattr(fn, "__qualname__", "?"))
    return fn


class Triangle(shapes.Shape):  ## def class Triangle; ref Shape; impl Triangle of Shape
    def __init__(self, base: float, height: float) -> None:  ## def method Triangle.__init__
        super().__init__("triangle")  ## ref __init__; call Triangle.__init__ -> Shape.__init__
        self.base, self.height = base, height

    def area(self) -> float:  ## def method Triangle.area
        return self.base * self.height / 2

    def summary(self) -> str:  ## def method Triangle.summary
        return self.describe()  ## ref describe; call Triangle.summary -> Shape.describe


class Inventory:  ## def class Inventory
    store: Repository[Shape]  ## ref Repository; ref Shape

    def __init__(self, store: Repository[Shape]) -> None:  ## def method Inventory.__init__; ref Repository; ref Shape
        self.store = store

    @registered  ## ref registered; annot registered on Inventory.add
    def add(self, shape: Shape) -> int:  ## def method Inventory.add; ref Shape
        return self.store.save(shape)  ## ref save; call Inventory.add -> Repository.save; call Inventory.add -> InMemoryRepository.save

    def largest(self) -> Shape | None:  ## def method Inventory.largest; ref Shape
        found = self.store.find(1)  ## ref find; call Inventory.largest -> Repository.find
        if found is not None and is_large(found):  ## ref is_large; call Inventory.largest -> is_large
            return found
        return None


async def fetch_area(shape: Shape) -> float:  ## def function fetch_area; ref Shape
    await asyncio.sleep(0)
    return shape.area()  ## ref area; call fetch_area -> Shape.area


@registered  ## ref registered; annot registered on report
def report(inventory: Inventory) -> str:  ## def function report; ref Inventory
    def line(shape: Shape) -> str:  ## def function report.line; ref Shape
        return shape.describe()  ## ref describe; call report.line -> Shape.describe

    biggest = inventory.largest()  ## ref largest; call report -> Inventory.largest
    return line(biggest) if biggest else "-"  ## ref line; call report -> report.line


def main() -> None:  ## def function main
    inventory = Inventory(InMemoryRepository())  ## ref Inventory; ref InMemoryRepository; call main -> Inventory; call main -> InMemoryRepository
    inventory.add(Circle(2.0))  ## ref add; ref Circle; call main -> Inventory.add; call main -> Circle
    inventory.add(Square(12.0))  ## ref add; ref Square; call main -> Square
    inventory.add(Triangle(3.0, 4.0))  ## ref add; ref Triangle; call main -> Triangle
    print(Triangle(1.0, 1.0).summary())  ## ref Triangle; ref summary; call main -> Triangle.summary
    small = Circle.unit()  ## ref Circle; ref unit; call main -> Circle.unit
    print(small.diameter, total_area([small, Square(1.0)]))  ## ref diameter; ref total_area; ref Square; call main -> total_area
    print(asyncio.run(fetch_area(small)), report(inventory))  ## ref fetch_area; ref report; call main -> fetch_area; call main -> report
    here = geo.Point(3.0, 4.0)  ## ref Point; call main -> Point
    print(geo.span(here), geo.midpoint((here, geo.Point.origin())))  ## ref span; ref midpoint; ref Point; ref origin; call main -> span; call main -> midpoint; call main -> Point.origin
    print(first([core_version()]), geo.Grid().cell().neighbors())  ## ref first; ref core_version; ref Grid; ref cell; ref neighbors; call main -> first; call main -> core_version


if __name__ == "__main__":
    main()  ## ref main
