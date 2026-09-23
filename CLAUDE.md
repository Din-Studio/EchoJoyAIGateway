# Development Philosophy

## Fundamental Principles

- **Declarative Design and Code**:
  - When the primary goal is to design or refactor architecture, module boundaries, or interface contracts, you MUST use declarative representations at the relevant abstraction layer. Explicitly describe responsibilities, inputs and outputs, constraints, dependencies, and composition. This requirement does not extend to every internal implementation detail.
  - For routine feature implementation and local bug fixes, prefer existing project structures and a direct implementation that meets the requirements. Adding a function, type, or API endpoint alone does not justify introducing additional abstractions.
  - Use existing mechanisms such as types, schemas, and component composition as needed. Keep contracts understandable without tracing internal execution steps. Simple, clear conditionals, loops, and sequential calls may remain in concrete implementations.
  - Introduce rule tables, state-transition tables, or configuration-driven mechanisms only when current requirements contain explicit rules, state transitions, or composition relationships and the representation reduces the cost of understanding and modifying them.
  - **Do not add abstraction layers, generic engines, or DSLs solely to satisfy this principle. If a representation requires extra machinery, prefer a simpler representation. Additional complexity must be justified by current requirements**.
- **IMPORTANT — Composition over Inheritance**: Small components that combine together
- **Immutability**: Predictable and traceable state via Zustand
- **Type Safety**: Strict TypeScript without `any` throughout the project
- **Performance Awareness**: Account for known performance constraints during design. Drive specific optimizations with explicit requirements or measurements.
- **KISS (Keep It Simple)**: Use the simplest design that satisfies the required contracts and behavior. Declarative design must clarify intent without adding unnecessary indirection.

## Code Practices

- **Single Responsibility**: Each module has a single responsibility
- **DRY (Don't Repeat Yourself)**: Reuse through composition
- **YAGNI (You Aren't Gonna Need It)**: No premature abstractions
- **Fail Fast**: Validation and explicit errors immediately

## Communication Style

- **IMPORTANT — Language**: Always reply in the user's language.
- **Action**: Lead with the next action; end with one task under 2 minutes. Number steps, one bounded action each; limit lists to 5 items.
- **Progress**: Each response states the current step (`2/5`), completed results, and concrete time estimates (`~15 min`).
- **Clarity**: State error causes and fixes plainly. Skip repeated apologies, recaps, restated requests, and pleasantries.
- **Focus**: Finish current work before pursuing adjacent ideas.

Relax the format for explanations, destructive-action confirmations, stalled debugging that needs explicit assumptions, ambiguous requests, or when it hinders the task.
