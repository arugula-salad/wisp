const cards = [...document.querySelectorAll('.gallery-card')];
const filters = [...document.querySelectorAll('[data-filter]')];
const count = document.querySelector('#gallery-count');
document.querySelector('.gallery-controls').hidden = false;
function filterGallery(category) {
  let visible = 0;
  cards.forEach(card => {
    card.hidden = category !== 'all' && card.dataset.category !== category;
    if (!card.hidden) visible++;
  });
  filters.forEach(button => {
    const active = button.dataset.filter === category;
    button.classList.toggle('active', active);
    button.setAttribute('aria-pressed', String(active));
  });
  count.textContent = `${visible} views to explore`;
}
filters.forEach(button => button.addEventListener('click', () => filterGallery(button.dataset.filter)));
filterGallery('all');

const lightbox = document.querySelector('#lightbox');
const largeImage = document.querySelector('#lightbox-img');
const caption = document.querySelector('#lightbox-caption');
if (typeof lightbox.showModal === 'function') {
  document.querySelectorAll('.screenshot-link').forEach(link => {
    link.addEventListener('click', event => {
      if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
      event.preventDefault();
      largeImage.src = link.href;
      largeImage.alt = link.querySelector('img').alt;
      caption.textContent = link.dataset.caption;
      document.querySelector('#original-image').href = link.href;
      lightbox.showModal();
      document.querySelector('.lightbox-image').scrollTop = 0;
    });
  });
  document.querySelector('#close-lightbox').addEventListener('click', () => lightbox.close());
  lightbox.addEventListener('click', event => {
    const bounds = lightbox.getBoundingClientRect();
    if (event.target === lightbox && (event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom)) lightbox.close();
  });
}
if (navigator.clipboard && window.isSecureContext) {
  document.querySelectorAll('[data-copy]').forEach(button => {
    button.hidden = false;
    button.addEventListener('click', async () => {
      const status = document.querySelector('#copy-status');
      try {
        await navigator.clipboard.writeText(document.getElementById(button.dataset.copy).textContent);
        button.textContent = 'Copied ✓';
        status.textContent = 'Commands copied to clipboard.';
      } catch {
        button.textContent = 'Select code';
        status.textContent = 'Copy was unavailable. Select and copy the commands manually.';
        const range = document.createRange();
        range.selectNodeContents(document.getElementById(button.dataset.copy));
        const selection = window.getSelection();
        selection.removeAllRanges();
        selection.addRange(range);
      }
      setTimeout(() => { button.textContent = 'Copy'; }, 2500);
    });
  });
}
