pub mod geometry;
pub mod shapes;
pub mod storage;

pub fn crate_version() -> &'static str { //# def function crate_version
    "1.0"
}

#[macro_export] //# annot macro_export on square
macro_rules! square { //# def macro square
    ($x:expr) => {
        $x * $x
    };
}

#[repr(C)]
pub union Bits { //# def struct Bits
    pub int: u32,
    pub float: f32,
}

extern "C" {
    pub fn abs(input: i32) -> i32; //# def function abs
}
