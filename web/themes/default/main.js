import './public.css';
import {mountPublicPage} from '../../src/public/page.js';
import {createCard, skeletonCards} from './cards.js';
import {createConstellation} from './constellation.js';

mountPublicPage({createCard, skeletonCards, createConstellation});
