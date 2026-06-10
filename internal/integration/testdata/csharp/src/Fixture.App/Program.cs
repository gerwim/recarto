using System;
using Fixture.Core; //# ref Fixture.Core
using Fixture.Core.Geometry; //# ref Fixture.Core.Geometry
using Fixture.Core.Shapes; //# ref Fixture.Core.Shapes
using Fixture.Core.Storage; //# ref Fixture.Core.Storage

namespace Fixture.App; //# def namespace Fixture.App

public static class Program //# def class Fixture.App.Program
{
    public static void Main() //# def method Fixture.App.Program.Main
    {
        RunGeometry(); //# ref RunGeometry; call Main -> RunGeometry
        RunShapes(); //# ref RunShapes; call Main -> RunShapes
        RunStorage(); //# ref RunStorage; call Main -> RunStorage
        var (lo, hi) = SortPair(9, 4); //# ref SortPair; call Main -> SortPair
        Console.WriteLine($"{lo} {hi}");
    }

    public static void RunGeometry() //# def method Fixture.App.Program.RunGeometry
    {
        GeoPoint home = new GeoPoint(52.0, 4.3); //# ref GeoPoint; call RunGeometry -> GeoPoint
        GeoPoint office = new GeoPoint(52.1, 4.5); //# ref GeoPoint
        double d = GeoMath.Distance(home, office); //# ref GeoMath; ref Distance; call RunGeometry -> GeoMath.Distance
        Pixel origin = new Pixel(0, 0); //# ref Pixel
        Pixel corner = origin with { X = 9 }; //# ref Pixel
        double pd = GeoMath.Distance(origin, corner); //# ref GeoMath; ref Distance
        Quadrant q = GeoMath.QuadrantOf(home); //# ref Quadrant; ref GeoMath; ref QuadrantOf; call RunGeometry -> QuadrantOf
        int x = 1, y = 2;
        GeoMath.Swap<int>(ref x, ref y); //# ref GeoMath; ref Swap; call RunGeometry -> Swap
        GeoPoint[] pts = { home, office }; //# ref GeoPoint
        var (min, max) = GeoMath.BoundingBox(pts); //# ref GeoMath; ref BoundingBox; call RunGeometry -> BoundingBox
        string recordName = nameof(GeoPoint); //# ref GeoPoint
        Console.WriteLine($"{d} {pd} {q} {x} {y} {min} {max} {recordName}");
    }

    public static void RunShapes() //# def method Fixture.App.Program.RunShapes
    {
        Circle circle = new Circle("unit", 1.0); //# ref Circle; call RunShapes -> Circle
        string label = circle.Describe(); //# ref Describe; call RunShapes -> Circle.Describe
        bool large = circle.IsLarge(); //# ref IsLarge; call RunShapes -> IsLarge
        IShape shape = circle; //# ref IShape
        double extra = 0;
        if (shape is Margin m) //# ref Margin
        {
            extra = m.Area(); //# ref Area
        }
        Catalog cat = new Catalog(); //# ref Catalog
        cat.Register(circle); //# ref Register; call RunShapes -> Register
        cat.Register(new Margin { Width = 200.0 }); //# ref Register; ref Margin; ref Width
        Catalog combined = cat + cat; //# ref Catalog
        IShape? hit = cat.FindByName(label); //# ref IShape; ref FindByName; call RunShapes -> FindByName
        int big = cat.CountLargeShapes(); //# ref CountLargeShapes; call RunShapes -> CountLargeShapes
        Console.WriteLine($"{label} {large} {extra} {combined.Newest} {hit} {big}"); //# ref Newest
    }

    public static void RunStorage() //# def method Fixture.App.Program.RunStorage
    {
        Circle circle = new Circle("disk", 9.0); //# ref Circle
        InMemoryRepository<Circle> repo = new InMemoryRepository<Circle>(4); //# ref InMemoryRepository; ref Circle; call RunStorage -> InMemoryRepository.InMemoryRepository
        repo.Save(circle); //# ref Save; call RunStorage -> InMemoryRepository.Save
        Circle? found = repo.Find(1); //# ref Circle; ref Find; call RunStorage -> InMemoryRepository.Find
        IRepository<string> names = new CachedRepository(); //# ref IRepository; ref CachedRepository; call RunStorage -> CachedRepository
        names.Save(nameof(Program)); //# ref Save; ref Program; call RunStorage -> IRepository.Save
        Console.WriteLine($"{found} {names.Find(0)}"); //# ref Find
    }

    public static (int Lo, int Hi) SortPair(int a, int b) //# def method Fixture.App.Program.SortPair
    {
        int Smaller(int u, int v) => u < v ? u : v;
        int lo = Smaller(a, b);
        int hi = a + b - lo;
        return (Lo: lo, Hi: hi);
    }
}
