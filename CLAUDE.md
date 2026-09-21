# Development Philosophy

## Fundamental Principles

- **Declarative Code**: Express what to do, not how to do it
- **Composition over Inheritance**: Small components that combine together
- **Immutability**: Predictable and traceable state via Zustand
- **Type Safety**: Strict TypeScript without `any` throughout the project
- **Performance First**: Optimizations from the start, not afterwards
- **KISS (Keep It Simple)**: Simplicity over complexity, always

## Code Practices

- **Single Responsibility**: Each module has a single responsibility
- **DRY (Don't Repeat Yourself)**: Reuse through composition
- **YAGNI (You Aren't Gonna Need It)**: No premature abstractions
- **Fail Fast**: Validation and explicit errors immediately

## Communication Style

Optimize every response for a reader with limited working memory:

- ***ALWAYS reply in the user's own language*** — non-negotiable, applies to every response
- **Action First**: Open with the immediate next action, never with context or preamble
- **Numbered Steps**: One bounded, single action per step; cap visible lists at 5 items
- **Progress Restated**: Say where we are (`step 2/5`) in every response — never assume recall
- **Time Ballparks**: Give concrete estimates (`~15 min`), not "quick" or "a while"
- **Wins Visible**: Name what concretely got done, not just what remains
- **Errors Plain**: State cause and fix matter-of-factly; no apology spirals
- **No Tangents**: Finish current work before raising anything adjacent; park ideas, don't chase them
- **No Filler**: Skip recaps, restated requests, and closing pleasantries
- **End Small**: Close with one task completable in under 2 minutes

Exceptions — drop these rules when: the user asks for an explanation, a destructive
action needs confirmation first, a debug spiral needs assumptions named out loud, the
request is genuinely ambiguous, or the rules would fight the actual task.
