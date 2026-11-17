(() => {
  const menu = document.querySelector('.menu-toggle');
  const navigation = document.querySelector('#mobile-nav');
  function closeMenu() {
    menu.setAttribute('aria-expanded', 'false');
    menu.setAttribute('aria-label', 'Open navigation');
    navigation.hidden = true;
  }
  menu.addEventListener('click', () => {
    const open = menu.getAttribute('aria-expanded') !== 'true';
    menu.setAttribute('aria-expanded', String(open));
    menu.setAttribute('aria-label', open ? 'Close navigation' : 'Open navigation');
    navigation.hidden = !open;
  });
  navigation.addEventListener('click', (event) => {
    if (event.target.closest('a')) closeMenu();
  });
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && !navigation.hidden) {
      closeMenu();
      menu.focus();
    }
  });
  window.matchMedia('(min-width: 801px)').addEventListener('change', closeMenu);

  const tabs = [...document.querySelectorAll('[role="tab"]')];
  function selectTab(tab, focus = false) {
    tabs.forEach((item) => {
      const selected = item === tab;
      item.setAttribute('aria-selected', String(selected));
      item.tabIndex = selected ? 0 : -1;
      document.getElementById(item.getAttribute('aria-controls')).hidden = !selected;
    });
    if (focus) tab.focus();
  }
  tabs.forEach((tab, index) => {
    tab.addEventListener('click', () => selectTab(tab));
    tab.addEventListener('keydown', (event) => {
      let next;
      if (event.key === 'ArrowRight') next = tabs[(index + 1) % tabs.length];
      if (event.key === 'ArrowLeft') next = tabs[(index - 1 + tabs.length) % tabs.length];
      if (event.key === 'Home') next = tabs[0];
      if (event.key === 'End') next = tabs[tabs.length - 1];
      if (next) {
        event.preventDefault();
        selectTab(next, true);
      }
    });
  });

  let toastTimer;
  const toast = document.querySelector('#copy-status');
  function notify(message) {
    clearTimeout(toastTimer);
    toast.textContent = message;
    toast.classList.add('visible');
    toastTimer = setTimeout(() => toast.classList.remove('visible'), 3500);
  }
  document.querySelectorAll('[data-copy]').forEach((button) => {
    button.addEventListener('click', async () => {
      const code = document.getElementById(`code-${button.dataset.copy}`);
      try {
        await navigator.clipboard.writeText(code.textContent.trim());
        notify('Commands copied. Your terminal is next.');
      } catch {
        // Keep the commands selectable when clipboard access is unavailable.
        const range = document.createRange();
        range.selectNodeContents(code);
        const selection = window.getSelection();
        selection.removeAllRanges();
        selection.addRange(range);
        notify('Clipboard unavailable. Commands selected; copy them manually.');
      }
    });
  });

  const run = document.querySelector('#run-demo');
  const output = document.querySelector('#demo-output');
  const status = document.querySelector('#demo-status');
  const caption = document.querySelector('#demo-caption');
  const stages = [...document.querySelectorAll('[data-stage]')];
  let timers = [];
  function stage(current) {
    stages.forEach((element, index) => {
      element.classList.toggle('current', index === current);
      element.classList.toggle('complete', index < current);
      element.querySelector('.stage-icon').textContent = index < current ? '✓' : `0${index + 1}`;
    });
  }
  const frames = [
    { stage: 0, status: 'Allocating', text: '[allocating] Creating a fresh workspace…' },
    { stage: 0, status: 'Workspace ready', text: '[ready] Workspace allocated. TTL: 20 minutes.' },
    { stage: 1, status: 'Executing', text: '[checkout] Exact commit checked out. Running go test ./...' },
    { stage: 1, status: 'Tests passed', text: 'ok   example/hello   (cached)', success: true },
    { stage: 2, status: 'Cleaning up', text: '[cleanup] Workspace deletion requested.' },
    { stage: 3, status: 'Demo complete', text: '✓ Command succeeded. Kubernetes handles the cleanup.', success: true },
  ];
  run.addEventListener('click', () => {
    timers.forEach(clearTimeout);
    timers = [];
    run.disabled = true;
    run.textContent = 'Running…';
    output.replaceChildren();
    caption.textContent = 'Simulated output. No commands are being executed.';
    const interval = window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 0 : 650;
    frames.forEach((frame, index) => {
      timers.push(setTimeout(() => {
        stage(frame.stage);
        status.textContent = frame.status;
        const line = document.createElement('div');
        line.textContent = frame.text;
        if (frame.success) line.className = 'terminal-success';
        output.appendChild(line);
        if (index === frames.length - 1) {
          run.disabled = false;
          run.textContent = 'Run it again ↻';
          caption.textContent = 'Example complete. Your code could be next.';
        }
      }, interval * index));
    });
  });
  window.addEventListener('pagehide', () => timers.forEach(clearTimeout));
})();
