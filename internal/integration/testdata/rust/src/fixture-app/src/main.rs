use fixture_core::geometry::{bounding_box, distance, quadrant_of, swap, units, GeoPoint, Quadrant}; //# ref bounding_box; ref distance; ref quadrant_of; ref swap; ref units; ref GeoPoint; ref Quadrant
use fixture_core::shapes::{is_large, label_of, Circle, Resizable, Shape, Square}; //# ref is_large; ref label_of; ref Circle; ref Resizable; ref Shape; ref Square
use fixture_core::storage::{InMemoryRepository, Repository}; //# ref InMemoryRepository; ref Repository
use fixture_core::square; //# ref square

fn main() { //# def function main
    run_geometry(); //# ref run_geometry; call main -> run_geometry
    run_shapes(); //# ref run_shapes; call main -> run_shapes
    run_storage(); //# ref run_storage; call main -> run_storage
    println!("{}", fixture_core::crate_version()); //# ref crate_version; call main -> crate_version
}

fn run_geometry() { //# def function run_geometry
    let home = GeoPoint::new(52.0, 4.3); //# ref GeoPoint; ref new; call run_geometry -> GeoPoint.new
    let office = GeoPoint::origin(); //# ref GeoPoint; ref origin; call run_geometry -> origin
    let d = distance(&home, &office); //# ref distance; call run_geometry -> distance
    let q = quadrant_of(home); //# ref quadrant_of; call run_geometry -> quadrant_of
    let (lo, hi) = bounding_box(&[home, office]); //# ref bounding_box; call run_geometry -> bounding_box
    let mut a = 1;
    let mut b = 2;
    swap::<i32>(&mut a, &mut b); //# ref swap; call run_geometry -> swap
    let m: units::Meters = units::Meters(1500.0); //# ref units; ref Meters; call run_geometry -> Meters
    assert_eq!(q, Quadrant::NorthEast); //# ref Quadrant; ref NorthEast
    println!("{d} {lo:?} {hi:?} {a} {b} {} {}", m.to_km(), square!(3)); //# ref to_km; ref square; call run_geometry -> to_km
    println!("{}", sort_pair(9, 4).0); //# ref sort_pair; call run_geometry -> sort_pair
}

fn run_shapes() { //# def function run_shapes
    let mut c = Circle::new(2.0); //# ref Circle; ref new; call run_shapes -> Circle.new
    c.scale(3.0); //# ref scale; call run_shapes -> Circle.scale
    let s = Square(12.0); //# ref Square; call run_shapes -> Square
    let text = c.describe(); //# ref describe
    let big = is_large(&s); //# ref is_large; call run_shapes -> is_large
    let u: Circle = Circle::unit(); //# ref Circle; ref unit; call run_shapes -> unit
    println!("{text} {big} {} {}", u.area(), label_of(s)); //# ref area; ref label_of; call run_shapes -> Circle.area; call run_shapes -> label_of
}

fn run_storage() { //# def function run_storage
    let mut repo: InMemoryRepository<Circle> = InMemoryRepository::with_capacity(4); //# ref InMemoryRepository; ref Circle; ref with_capacity; call run_storage -> with_capacity
    let id = repo.save(Circle::unit()); //# ref save; ref Circle; ref unit; call run_storage -> InMemoryRepository.save
    let found = repo.find(id).map(|c| c.area()); //# ref find; ref area; call run_storage -> InMemoryRepository.find
    let total = store_all(&mut repo, vec![Circle::new(1.0)]); //# ref store_all; ref Circle; ref new; call run_storage -> store_all
    println!("{found:?} {total} {}", repo.count()); //# ref count; call run_storage -> count
}

fn store_all(repo: &mut dyn Repository<Circle>, items: Vec<Circle>) -> usize { //# def function store_all; ref Repository; ref Circle
    let mut last = 0;
    for item in items {
        last = repo.save(item); //# ref save; call store_all -> Repository.save; call store_all -> InMemoryRepository.save
    }
    last
}

fn sort_pair(a: i32, b: i32) -> (i32, i32) { //# def function sort_pair
    fn smaller(u: i32, v: i32) -> i32 { //# def function sort_pair.smaller
        if u < v {
            u
        } else {
            v
        }
    }
    let lo = smaller(a, b); //# ref smaller; call sort_pair -> smaller
    (lo, a + b - lo)
}
