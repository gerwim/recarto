using System.Collections.Generic;
using Fixture.Core.Auditing; //# ref Fixture.Core.Auditing
using Fixture.Core.Shapes; //# ref Fixture.Core.Shapes

namespace Fixture.Core //# def namespace Fixture.Core
{
    [Audit("catalog")] //# ref Audit; annot Audit on Catalog
    public partial class Catalog //# def class Fixture.Core.Catalog
    {
        private readonly List<IShape> shapes = new(); //# ref IShape

        public Entry? Newest { get; set; } //# def property Fixture.Core.Catalog.Newest; ref Entry

        public void Register(IShape shape) //# def method Fixture.Core.Catalog.Register; ref IShape
        {
            shapes.Add(shape);
            Newest = new Entry { Label = shape.Describe() }; //# ref Newest; ref Entry; ref Label; ref Describe; call Register -> IShape.Describe
        }

        public IShape this[int i] => shapes[i]; //# ref IShape

        public static Catalog operator +(Catalog left, Catalog right) //# ref Catalog
        {
            var merged = new Catalog(); //# ref Catalog
            merged.shapes.AddRange(left.shapes);
            merged.shapes.AddRange(right.shapes);
            return merged;
        }

        ~Catalog() //# ref Catalog
        {
            shapes.Clear();
        }

        public class Entry //# def class Fixture.Core.Catalog.Entry
        {
            public string Label { get; set; } = ""; //# def property Fixture.Core.Catalog.Entry.Label
        }
    }
}
