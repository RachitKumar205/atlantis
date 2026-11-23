// The four-step walkthrough: tabs over one terminal window, which replays
// each step's command and output.
//
// Without motion (prefers-reduced-motion, or no script) every panel shows its
// whole output and the tabs only switch panels.
//
// With motion the four steps play in a loop while the section is on screen. A
// click on a step restarts the loop from that step. Only the toggle in the
// window bar stops it.

type Action = [run: () => void, ms: number];

const CHAR_MS = 38;
const LINE_MS = 55;
const HOLD_MS = 3400;

// Clock is a timer that stops counting while it is paused, so a paused demo
// resumes mid-line and the rail's progress stops with it.
class Clock {
  private paused = true;
  private timer = 0;
  private wake: (() => void) | null = null;
  private left = 0;
  private armedAt = 0;
  private spent = 0;

  sleep(ms: number): Promise<void> {
    this.cancel();
    return new Promise((resolve) => {
      this.wake = resolve;
      this.left = ms;
      if (!this.paused) this.arm();
    });
  }

  // now is the unpaused time this clock has spent sleeping, in ms.
  now(): number {
    const running = this.wake && !this.paused ? performance.now() - this.armedAt : 0;
    return this.spent + Math.min(running, this.left);
  }

  setPaused(paused: boolean): void {
    if (paused === this.paused) return;
    this.paused = paused;
    if (!this.wake) return;
    if (paused) {
      clearTimeout(this.timer);
      const ran = performance.now() - this.armedAt;
      this.spent += ran;
      this.left = Math.max(0, this.left - ran);
    } else {
      this.arm();
    }
  }

  // cancel wakes a pending sleep at once; the run that awaited it checks its
  // token and stops.
  cancel(): void {
    clearTimeout(this.timer);
    const wake = this.wake;
    this.wake = null;
    wake?.();
  }

  private arm(): void {
    this.armedAt = performance.now();
    this.timer = window.setTimeout(() => {
      this.spent += this.left;
      const wake = this.wake;
      this.wake = null;
      wake?.();
    }, this.left);
  }
}

class Panel {
  readonly el: HTMLElement;
  private readonly typed: HTMLElement;
  private readonly command: string;
  private readonly lines: HTMLElement[];
  private readonly wait: number;
  private readonly dialog: HTMLElement | null;
  private readonly toast: HTMLElement | null;

  constructor(el: HTMLElement) {
    this.el = el;
    this.typed = el.querySelector<HTMLElement>(".flow__typed")!;
    this.command = this.typed.dataset.command ?? "";
    this.lines = [...el.querySelectorAll<HTMLElement>(".flow__out .line")];
    this.wait = Number(el.dataset.wait ?? 300);
    this.dialog = el.querySelector<HTMLElement>(".flow__dialog");
    this.toast = el.querySelector<HTMLElement>(".flow__toast");
  }

  // complete shows the whole step, as the page renders without the script.
  complete(): void {
    this.el.style.minHeight = "";
    this.typed.textContent = this.command;
    for (const line of this.lines) line.classList.remove("is-pending");
    this.closeDialog();
  }

  // script lists what the step prints and the pause after each part. The
  // last pause holds the finished output before the next step.
  script(caret: HTMLElement): Action[] {
    const actions: Action[] = [
      [() => this.reset(caret), 400],
      ...[...this.command].map((_, i): Action => [() => (this.typed.textContent = this.command.slice(0, i + 1)), CHAR_MS]),
      [() => caret.classList.remove("is-typing"), this.wait],
    ];
    this.lines.forEach((line, i) => {
      actions.push([() => this.show(line, caret), line.textContent ? LINE_MS : LINE_MS / 2]);
      if (this.dialog && line.textContent?.startsWith("ℹ")) actions.push(...this.approval());
      if (i === this.lines.length - 1) actions.push([() => undefined, HOLD_MS]);
    });
    return actions;
  }

  private reset(caret: HTMLElement): void {
    this.el.style.minHeight = "";
    for (const line of this.lines) line.classList.remove("is-pending");
    this.el.style.minHeight = `${this.el.offsetHeight}px`;
    for (const line of this.lines) line.classList.add("is-pending");
    this.typed.textContent = "";
    this.closeDialog();
    caret.classList.add("is-typing");
    this.typed.after(caret);
  }

  private show(line: HTMLElement, caret: HTMLElement): void {
    line.classList.remove("is-pending");
    line.append(caret);
  }

  // approval replays the console's approve dialog while tide apply waits.
  private approval(): Action[] {
    const dialog = this.dialog!;
    const slots = [...dialog.querySelectorAll<HTMLElement>(".flow__slot")];
    const button = dialog.querySelector<HTMLElement>("[data-approve]")!;
    return [
      [() => undefined, 900],
      [() => dialog.classList.add("is-open"), 600],
      ...slots.map((slot): Action => [() => (slot.textContent = slot.dataset.digit ?? ""), 120]),
      [() => button.classList.add("is-pressed"), 160],
      [() => (button.textContent = "Checking code…"), 900],
      [() => { dialog.classList.remove("is-open"); this.toast?.classList.add("is-open"); }, 1400],
      [() => this.toast?.classList.remove("is-open"), 300],
    ];
  }

  private closeDialog(): void {
    if (!this.dialog) return;
    this.dialog.classList.remove("is-open");
    this.toast?.classList.remove("is-open");
    for (const slot of this.dialog.querySelectorAll<HTMLElement>(".flow__slot")) slot.textContent = "";
    const button = this.dialog.querySelector<HTMLElement>("[data-approve]")!;
    button.classList.remove("is-pressed");
    button.textContent = "Approve";
  }
}

export function initFlow(flow: HTMLElement): void {
  const tabs = [...flow.querySelectorAll<HTMLButtonElement>('[role="tab"]')];
  const panels = tabs.map((tab) => new Panel(document.getElementById(tab.getAttribute("aria-controls") ?? "")!));
  const strip = flow.querySelector<HTMLElement>(".flow__steps")!;
  const toggle = flow.querySelector<HTMLButtonElement>("[data-flow-toggle]")!;
  const caret = document.createElement("span");
  caret.className = "flow__caret";
  caret.setAttribute("aria-hidden", "true");

  const motion = !matchMedia("(prefers-reduced-motion: reduce)").matches;
  const clock = new Clock();
  let current = 0;
  let userPaused = false;
  let onScreen = false;
  let started = false;
  let token = 0;
  let stepStart = 0;
  let stepLength = 1;

  const select = (index: number) => {
    current = index;
    tabs.forEach((tab, i) => {
      tab.setAttribute("aria-selected", String(i === index));
      tab.tabIndex = i === index ? 0 : -1;
      tab.classList.toggle("is-current", i === index);
      tab.classList.toggle("is-done", i < index);
      tab.style.removeProperty("--p");
      panels[i].el.classList.toggle("is-current", i === index);
      if (i !== index) panels[i].complete();
    });
    if (strip.scrollWidth > strip.clientWidth) {
      const left = tabs[index].offsetLeft - parseFloat(getComputedStyle(strip).paddingLeft);
      strip.scrollTo({ left, behavior: motion ? "smooth" : "auto" });
    }
  };

  const syncClock = () => clock.setPaused(userPaused || !onScreen || document.hidden);

  const syncToggle = () => {
    toggle.classList.toggle("is-paused", userPaused);
    toggle.setAttribute("aria-label", userPaused ? "Play the demo" : "Pause the demo");
  };

  // run plays from one step to the next until a newer run replaces it.
  const run = async (from: number) => {
    const mine = ++token;
    clock.cancel();
    for (let index = from; ; index = (index + 1) % panels.length) {
      select(index);
      const actions = panels[index].script(caret);
      stepLength = actions.reduce((sum, [, ms]) => sum + ms, 0);
      stepStart = clock.now();
      for (const [act, ms] of actions) {
        act();
        await clock.sleep(ms);
        if (mine !== token) return;
      }
    }
  };

  // The rail of the playing step fills as it plays. The last step's rail ends
  // as the loop returns to the first step.
  const frame = () => {
    if (started) {
      const p = Math.min(1, (clock.now() - stepStart) / stepLength);
      tabs[current].style.setProperty("--p", p.toFixed(4));
    }
    requestAnimationFrame(frame);
  };

  tabs.forEach((tab, i) => {
    const choose = () => {
      if (!motion) {
        select(i);
        return;
      }
      started = true;
      userPaused = false;
      onScreen = true;
      syncToggle();
      syncClock();
      void run(i);
    };
    tab.addEventListener("click", choose);
    tab.addEventListener("keydown", (event) => {
      const last = tabs.length - 1;
      const keys: Record<string, number> = {
        ArrowRight: i === last ? 0 : i + 1,
        ArrowLeft: i === 0 ? last : i - 1,
        Home: 0,
        End: last,
      };
      const next = keys[event.key];
      if (next === undefined) return;
      event.preventDefault();
      tabs[next].focus();
      tabs[next].click();
    });
  });

  flow.dataset.ready = "";
  if (!motion) {
    select(0);
    return;
  }

  toggle.hidden = false;
  toggle.addEventListener("click", () => {
    onScreen = true;
    userPaused = !userPaused;
    syncToggle();
    syncClock();
    if (!started) {
      started = true;
      void run(0);
    }
  });

  new IntersectionObserver(
    ([entry]) => {
      onScreen = entry.isIntersecting;
      syncClock();
      if (onScreen && !started) {
        started = true;
        void run(0);
      }
    },
    { threshold: 0.25 },
  ).observe(flow);
  document.addEventListener("visibilitychange", syncClock);

  panels[0].script(caret)[0][0]();
  syncToggle();
  requestAnimationFrame(frame);
}
