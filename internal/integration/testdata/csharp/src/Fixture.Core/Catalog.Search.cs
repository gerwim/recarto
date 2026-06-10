using Fixture.Core.Auditing; //# ref Fixture.Core.Auditing
using Fixture.Core.Shapes; //# ref Fixture.Core.Shapes

namespace Fixture.Core //# def namespace Fixture.Core
{
    public partial class Catalog //# def class Fixture.Core.Catalog
    {
        [Audit("search")] //# ref Audit; annot Audit on FindByName
        public IShape? FindByName(string name) //# def method Fixture.Core.Catalog.FindByName; ref IShape
        {
            foreach (IShape s in shapes) //# ref IShape
            {
                if (s.Describe() == name) //# ref Describe; call FindByName -> IShape.Describe
                {
                    return s;
                }
            }
            return null;
        }

        public int CountLargeShapes() //# def method Fixture.Core.Catalog.CountLargeShapes
        {
            int n = 0;
            foreach (IShape s in shapes) //# ref IShape
            {
                if (s.IsLarge()) //# ref IsLarge; call CountLargeShapes -> IsLarge
                {
                    n++;
                }
            }
            return n;
        }
    }
}
