using System;

namespace Fixture.Core.Shapes; //# def namespace Fixture.Core.Shapes

public interface IShape //# def interface Fixture.Core.Shapes.IShape
{
    double Area(); //# def method Fixture.Core.Shapes.IShape.Area
    string Describe(); //# def method Fixture.Core.Shapes.IShape.Describe
}

public interface IResizable : IShape //# def interface Fixture.Core.Shapes.IResizable; impl IResizable of IShape; ref IShape
{
    void Scale(double factor); //# def method Fixture.Core.Shapes.IResizable.Scale
}

public abstract record Shape(string Name) //# def record Fixture.Core.Shapes.Shape
{
    public abstract double Area(); //# def method Fixture.Core.Shapes.Shape.Area
}

public record Circle(string Name, double Radius) : Shape(Name), IShape //# def record Fixture.Core.Shapes.Circle; impl Circle of Shape; impl Circle of IShape; ref Shape; ref IShape
{
    public override double Area() => Math.PI * Radius * Radius; //# def method Fixture.Core.Shapes.Circle.Area
    public string Describe() => $"circle {Name} r={Radius}"; //# def method Fixture.Core.Shapes.Circle.Describe
}

public readonly struct Margin : IShape //# def struct Fixture.Core.Shapes.Margin; impl Margin of IShape; ref IShape
{
    public double Width { get; init; } //# def property Fixture.Core.Shapes.Margin.Width
    public double Area() => Width * Width; //# def method Fixture.Core.Shapes.Margin.Area; ref Width
    public string Describe() => "margin"; //# def method Fixture.Core.Shapes.Margin.Describe
}

public static class ShapeExtensions //# def class Fixture.Core.Shapes.ShapeExtensions
{
    public static bool IsLarge(this IShape shape) => shape.Area() > 100.0; //# def method Fixture.Core.Shapes.ShapeExtensions.IsLarge; ref IShape; ref Area; call IsLarge -> IShape.Area
}
