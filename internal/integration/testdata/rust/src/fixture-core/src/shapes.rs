pub trait Shape { //# def trait Shape
    fn area(&self) -> f64; //# def method Shape.area

    fn describe(&self) -> String { //# def method Shape.describe
        format!("shape with area {}", self.area()) //# ref area; call Shape.describe -> Shape.area
    }
}

pub trait Resizable: Shape { //# def trait Resizable; impl Resizable of Shape; ref Shape
    fn scale(&mut self, factor: f64); //# def method Resizable.scale
}

#[derive(Debug, Clone)]
pub struct Circle { //# def struct Circle
    pub radius: f64,
}

impl Circle { //# def impl Circle
    pub fn new(radius: f64) -> Circle { //# def method Circle.new; ref Circle
        Circle { radius } //# ref Circle
    }

    pub fn unit() -> Self { //# def method Circle.unit; implicit Circle
        Self::new(1.0) //# ref new; call Circle.unit -> Circle.new; implicit Circle
    }
}

impl Shape for Circle { //# def impl Circle; impl Circle of Shape; ref Shape
    fn area(&self) -> f64 { //# def method Circle.area
        std::f64::consts::PI * self.radius * self.radius
    }
}

impl Resizable for Circle { //# def impl Circle; impl Circle of Resizable; ref Resizable
    fn scale(&mut self, factor: f64) { //# def method Circle.scale
        self.radius *= factor;
    }
}

#[derive(Debug, Clone, Copy)]
pub struct Square(pub f64); //# def struct Square

impl Shape for Square { //# def impl Square; impl Square of Shape; ref Shape
    fn area(&self) -> f64 { //# def method Square.area
        self.0 * self.0
    }

    fn describe(&self) -> String { //# def method Square.describe
        format!("square {}", self.0) //# tok square
    }
}

pub fn is_large(shape: &dyn Shape) -> bool { //# def function is_large; ref Shape
    shape.area() > 100.0 //# ref area; call is_large -> Shape.area; call is_large -> Circle.area
}

pub fn area_of<S: Shape>(shape: &S) -> f64 { //# def function area_of; ref Shape
    shape.area() //# ref area
}

pub fn label_of(shape: impl Shape) -> String { //# def function label_of; ref Shape
    shape.describe() //# ref describe; call label_of -> Shape.describe
}

#[cfg(test)] //# annot cfg on tests
mod tests { //# def module tests
    use super::*;

    #[test] //# annot test on tests.circle_area
    fn circle_area() { //# def function tests.circle_area
        let c = Circle::new(1.0); //# ref Circle; ref new; call tests.circle_area -> Circle.new
        assert!(!is_large(&c)); //# ref is_large; call tests.circle_area -> is_large
        assert_eq!(Square(2.0).area(), 4.0); //# ref Square; ref area; call tests.circle_area -> Square
        assert!(super::area_of::<Circle>(&c) > 3.0); //# ref area_of; ref Circle; call tests.circle_area -> area_of
    }

    #[test] //# annot test on tests.square_area
    fn square_area() { //# def function tests.square_area
        let a = super::area_of::<Square>(&Square(2.0)); //# ref area_of; ref Square; call tests.square_area -> area_of; call tests.square_area -> Square
        assert_eq!(a, 4.0);
    }
}
