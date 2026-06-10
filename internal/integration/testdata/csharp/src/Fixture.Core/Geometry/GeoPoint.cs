using System;

namespace Fixture.Core.Geometry; //# def namespace Fixture.Core.Geometry

public record GeoPoint(double Lat, double Lon); //# def record Fixture.Core.Geometry.GeoPoint

public record struct Pixel(int X, int Y); //# def record Fixture.Core.Geometry.Pixel

public enum Quadrant //# def enum Fixture.Core.Geometry.Quadrant
{
    NorthEast, //# def enum_member Fixture.Core.Geometry.Quadrant.NorthEast
    NorthWest, //# def enum_member Fixture.Core.Geometry.Quadrant.NorthWest
    SouthEast, //# def enum_member Fixture.Core.Geometry.Quadrant.SouthEast
    SouthWest, //# def enum_member Fixture.Core.Geometry.Quadrant.SouthWest
}

public static class GeoMath //# def class Fixture.Core.Geometry.GeoMath
{
    public static double Distance(GeoPoint a, GeoPoint b) //# def method Fixture.Core.Geometry.GeoMath.Distance; ref GeoPoint
    {
        double dLat = a.Lat - b.Lat;
        double dLon = a.Lon - b.Lon;
        return Math.Sqrt(dLat * dLat + dLon * dLon);
    }

    public static double Distance(Pixel a, Pixel b) //# def method Fixture.Core.Geometry.GeoMath.Distance; ref Pixel
    {
        double dx = a.X - b.X;
        double dy = a.Y - b.Y;
        return Math.Sqrt(dx * dx + dy * dy);
    }

    [Obsolete("use the overloads above")] //# annot Obsolete on LegacyDistance
    public static double LegacyDistance(GeoPoint a, GeoPoint b) => Distance(a, b); //# def method Fixture.Core.Geometry.GeoMath.LegacyDistance; ref GeoPoint; ref Distance; call LegacyDistance -> GeoMath.Distance

    public static Quadrant QuadrantOf(GeoPoint p) //# def method Fixture.Core.Geometry.GeoMath.QuadrantOf; ref Quadrant; ref GeoPoint
    {
        if (p.Lat >= 0)
        {
            return p.Lon >= 0 ? Quadrant.NorthEast : Quadrant.NorthWest; //# ref Quadrant; ref NorthEast; ref NorthWest
        }
        return p.Lon >= 0 ? Quadrant.SouthEast : Quadrant.SouthWest; //# ref Quadrant; ref SouthEast; ref SouthWest
    }

    public static void Swap<T>(ref T a, ref T b) //# def method Fixture.Core.Geometry.GeoMath.Swap
    {
        (a, b) = (b, a);
    }

    public static (GeoPoint Min, GeoPoint Max) BoundingBox(GeoPoint[] pts) //# def method Fixture.Core.Geometry.GeoMath.BoundingBox; ref GeoPoint
    {
        GeoPoint min = pts[0]; //# ref GeoPoint
        GeoPoint max = pts[0]; //# ref GeoPoint
        foreach (GeoPoint p in pts) //# ref GeoPoint
        {
            if (Distance(p, min) > Distance(max, min)) //# ref Distance; call BoundingBox -> GeoMath.Distance
            {
                max = p;
            }
        }
        return (min, max);
    }
}
