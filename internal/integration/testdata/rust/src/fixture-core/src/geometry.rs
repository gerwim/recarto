#[derive(Debug, Clone, Copy, PartialEq)] //# annot derive on GeoPoint; annot Debug on GeoPoint
pub struct GeoPoint { //# def struct GeoPoint
    pub lat: f64,
    pub lon: f64,
}

impl GeoPoint { //# def impl GeoPoint
    pub fn new(lat: f64, lon: f64) -> Self { //# def method GeoPoint.new; implicit GeoPoint
        Self { lat, lon } //# implicit GeoPoint
    }

    pub fn origin() -> Self { //# def method GeoPoint.origin; implicit GeoPoint
        Self::new(0.0, 0.0) //# ref new; call GeoPoint.origin -> GeoPoint.new; implicit GeoPoint
    }
}

#[derive(Debug, PartialEq, Eq)]
pub enum Quadrant { //# def enum Quadrant
    NorthEast, //# def enum_member Quadrant.NorthEast
    NorthWest, //# def enum_member Quadrant.NorthWest
    South, //# def enum_member Quadrant.South
}

pub type Pair = (GeoPoint, GeoPoint); //# def type Pair; ref GeoPoint

pub fn distance(a: &GeoPoint, b: &GeoPoint) -> f64 { //# def function distance; ref GeoPoint
    let dlat = a.lat - b.lat;
    let dlon = a.lon - b.lon;
    (dlat * dlat + dlon * dlon).sqrt()
}

pub fn quadrant_of(p: GeoPoint) -> Quadrant { //# def function quadrant_of; ref GeoPoint; ref Quadrant
    if p.lat < 0.0 {
        Quadrant::South //# ref Quadrant; ref South
    } else if p.lon < 0.0 {
        Quadrant::NorthWest //# ref Quadrant; ref NorthWest
    } else {
        Quadrant::NorthEast //# ref Quadrant; ref NorthEast
    }
}

pub fn bounding_box(points: &[GeoPoint]) -> Pair { //# def function bounding_box; ref GeoPoint; ref Pair
    let mut lo = points[0];
    let mut hi = points[0];
    for p in points {
        lo = GeoPoint::new(lo.lat.min(p.lat), lo.lon.min(p.lon)); //# ref GeoPoint; ref new; call bounding_box -> GeoPoint.new
        hi = GeoPoint::new(hi.lat.max(p.lat), hi.lon.max(p.lon)); //# ref GeoPoint; ref new
    }
    (lo, hi)
}

pub fn swap<T: Copy>(a: &mut T, b: &mut T) { //# def function swap
    let t = *a;
    *a = *b;
    *b = t;
}

pub mod units { //# def module units
    pub struct Meters(pub f64); //# def struct units.Meters

    impl Meters { //# def impl units.Meters
        pub fn to_km(&self) -> f64 { //# def method units.Meters.to_km
            self.0 / 1000.0
        }
    }
}
