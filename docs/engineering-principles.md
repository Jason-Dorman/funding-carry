
# Quick Software Engineering Principles
 
## Core Mantra
**Maximize cohesion. Minimize coupling. Contain the impact of change.**
 
---
 
## SOLID Principles (Quick Check)
 
**S - Single Responsibility**
- One class = one reason to change
- Ask: "How many reasons could this change?"
 
**O - Open/Closed**
- Open for extension, closed for modification
- Add features without changing existing code
 
**L - Liskov Substitution**
- Subtypes must be fully substitutable for base types
- If it breaks when you swap, it violates LSP
 
**I - Interface Segregation**
- Many specific interfaces > one general interface
- Don't force clients to depend on unused methods
 
**D - Dependency Inversion**
- Depend on abstractions, not concretions
- High-level shouldn't depend on low-level
 
---
 
## Design Rules of Thumb
 
### Composition > Inheritance
- Default to HAS-A relationships
- Use IS-A only when true substitutability exists
- If using inheritance for code reuse → STOP, use composition
 
### Keep It Simple
- Each function/class does ONE thing
- Clear, meaningful names
- Small enough to reason about
- Minimal side effects
 
### Low Coupling
- Minimal dependencies between modules
- Interact through explicit, minimal interfaces
- Avoid shared global state
- No control flags that alter behavior
 
### High Cohesion
- Related things belong together
- Single focused purpose
- All methods support that purpose
- Changes stay localized

### Cyclomatic Complexity

- Score should be under 10 but 5 is more ideal
- If the score is higher than 10 is there opportunities to break the method into smaller logical chuncks 
- If the domain complexity requires a more complex method that is acceptable.
 
---
 
## Common Code Smells (Red Flags)
 
🚩 **Duplicated Code** - Same logic in multiple places
🚩 **Long Method** - Function does too many things
🚩 **Large Class** - Too many responsibilities (SRP violation)
🚩 **Feature Envy** - Uses another class's data more than its own
🚩 **Shotgun Surgery** - One change touches many files
🚩 **Divergent Change** - Class changes for different reasons
🚩 **Switch Statements** - Consider polymorphism instead
🚩 **Data Clumps** - Same group of variables appears together
 
---

 
## Testing Mindset
 
✅ Write tests FIRST (TDD when possible)
✅ Every change needs regression tests
✅ Make code testable (small, focused, minimal dependencies)
✅ Tests document intended behavior
❌ Don't skip tests - they enable safe refactoring
 
---
 
## Code Review Checklist
 
Before committing, ask:
- [ ] Does each class/function have ONE clear responsibility?
- [ ] Are responsibilities in the right places?
- [ ] Can I test this easily?
- [ ] Did I minimize coupling?
- [ ] Did I maximize cohesion?
- [ ] Does it follow SOLID principles?
- [ ] Are there obvious code smells?
- [ ] Will this change break other parts?
- [ ] Do I have tests for this?
 
---
 
## Refactoring Safety Rules
 
1. Understand current behavior FIRST
2. Add regression tests BEFORE changing
3. Make smallest possible changes
4. Run tests after EACH change
5. Commit frequently
 
**Never refactor without tests!**
 
---
 
## Key Questions to Ask
 
**Before writing:**
- What is this code's single responsibility?
- What decisions does it own?
- How will it be tested?
 
**During code review:**
- What is this trying to do at a business level?
- Do I see any code smells? Why?
- What responsibilities are mixed together?
- If this changes, what else might break?
 
**When debugging:**
- Is coupling too tight?
- Is cohesion too loose?
- Are responsibilities in the right places?
 
---
 
## Quick Refactoring Guide
 
**See duplicated code?** → Extract method/class
**Method too long?** → Break into smaller methods
**Class too large?** → Split responsibilities
**Feature envy?** → Move method to envied class
**Complex conditionals?** → Replace with polymorphism
**Primitive obsession?** → Create value objects
 
---
 
## Golden Rules
 
1. **Each component has ONE job**
2. **Favor composition over inheritance**
3. **Code to interfaces, not implementations**
4. **Make dependencies explicit**
5. **Test everything**
6. **Refactor continuously**
7. **Make intent clear**
8. **Minimize what you expose**
9. **Design for change**
10. **When in doubt, simplify**
 
---
 
## Warning Signs You're Going Wrong
 
⚠️ Adding new features requires changing lots of files
⚠️ Tests are hard to write
⚠️ Can't easily explain what a class does
⚠️ Class name has "AND" or "Manager" or "Helper"
⚠️ Methods frequently updated when requirements change
⚠️ Copy-pasting code to reuse it
⚠️ Deep inheritance hierarchies
⚠️ Lots of parameters being passed around
⚠️ Afraid to change code because it might break
 
---
 
## Emergency Principles (When Stuck)
 
1. **STOP** - Don't make it worse
2. **Write a test** - Understand current behavior
3. **Make it work** - Get it functioning
4. **Make it right** - Apply principles
 
---
 
## Remember
 
> Software systems are collections of decisions.  
> Good design makes those decisions visible, discussable, and changeable.
 
**The best code is code that's simple, elegant, and easy to change.**
