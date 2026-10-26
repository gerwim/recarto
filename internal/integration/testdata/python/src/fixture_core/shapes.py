from abc import ABC, abstractmethod


class Shape(ABC):  ## def class Shape
    def __init__(self, label: str) -> None:  ## def method Shape.__init__
        self.label = label

    @abstractmethod  ## annot abstractmethod on Shape.area
    def area(self) -> float: ...  ## def method Shape.area

    def describe(self) -> str:  ## def method Shape.describe
        return f"{self.label}: {self.area():.1f}"  ## ref area; call Shape.describe -> Shape.area


class Circle(Shape):  ## def class Circle; ref Shape; impl Circle of Shape
    def __init__(self, radius: float) -> None:  ## def method Circle.__init__
        super().__init__("circle")  ## ref __init__; call Circle.__init__ -> Shape.__init__
        self.radius = radius

    def area(self) -> float:  ## def method Circle.area
        return 3.14159 * self.radius**2

    @property  ## annot property on Circle.diameter
    def diameter(self) -> float:  ## def method Circle.diameter
        return 2 * self.radius

    @classmethod  ## annot classmethod on Circle.unit
    def unit(cls) -> "Circle":  ## def method Circle.unit; ref Circle
        return cls.from_radius(1.0)  ## ref from_radius; call Circle.unit -> Circle.from_radius

    @staticmethod  ## annot staticmethod on Circle.from_radius
    def from_radius(radius: float) -> "Circle":  ## def method Circle.from_radius; ref Circle
        return Circle(radius)  ## ref Circle; call Circle.from_radius -> Circle


class Square(Shape):  ## def class Square; ref Shape; impl Square of Shape
    def __init__(self, side: float) -> None:  ## def method Square.__init__
        super().__init__("square")  ## ref __init__; call Square.__init__ -> Shape.__init__
        self.side = side

    def area(self) -> float:  ## def method Square.area
        return self.side * self.side


def is_large(shape: Shape) -> bool:  ## def function is_large; ref Shape
    return shape.area() > 100.0  ## ref area; call is_large -> Shape.area; call is_large -> Circle.area


def total_area(items: list[Shape]) -> float:  ## def function total_area; ref Shape
    return sum(item.area() for item in items)  ## ref area
