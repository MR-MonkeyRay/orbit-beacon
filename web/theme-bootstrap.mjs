// Kept inline in the built HTML so the theme applies before styles paint.
export function themeBootstrap(consolePage) {
  const route = consolePage ? "if(location.pathname==='/login'||location.pathname==='/change-password')d.classList.add('route-login')" : '';
  return `<script>(function(d){try{var t=localStorage.getItem('orbitbeacon-theme');if(t==='light'||t==='dark')d.dataset.theme=t}catch(e){}${route}})(document.documentElement)</script>`;
}
