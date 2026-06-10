namespace Fixture.Core.Auditing; //# def namespace Fixture.Core.Auditing

public sealed class AuditAttribute : System.Attribute //# def class Fixture.Core.Auditing.AuditAttribute
{
    public AuditAttribute(string area) //# def constructor Fixture.Core.Auditing.AuditAttribute.AuditAttribute
    {
        Area = area; //# ref Area
    }

    public string Area { get; } //# def property Fixture.Core.Auditing.AuditAttribute.Area
}
